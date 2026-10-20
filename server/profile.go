package main

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	profilePrivateNotice = "🔒 This profile is private."

	profileGalleryMax = 6
)

func profileURL(username string) string {
	return instagramOrigin + "/" + url.PathEscape(username) + "/"
}

// unknownCount: no source gave the post count; the card omits it.
const unknownCount = -1

func profileStatsLine(p Profile) string {
	if p.MediaCount == unknownCount {
		return "👤 " + fmtCount(p.FollowerCount)
	}
	return "📝 " + fmtCount(p.MediaCount) + " 👤 " + fmtCount(p.FollowerCount)
}

func captionParagraphHTML(text string) string {
	text = normalizeCaption(text)
	if text == "" {
		return ""
	}
	return "<p>" + captionHTML(text) + "</p>"
}

type Profile struct {
	Username      string
	FullName      string
	Biography     string
	ProfilePic    string
	FollowerCount int
	MediaCount    int
	IsPrivate     bool
	IsVerified    bool
	RecentMedia   []ProfileMedia
}

type ProfileMedia struct {
	Thumbnail string
	Width     int
	Height    int
}

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`)

func validUsername(s string) bool { return usernameRE.MatchString(s) }

// Logged-out web app queries; neither returns the post count.
const (
	profileRootDocID  = "27981003384861049"
	profilePostsDocID = "27553725110923321"
)

// fetchProfile asks RootContent and PostsTab at once; the embed stands in only
// when RootContent is unreachable.
func (a *App) fetchProfile(ctx context.Context, username string) (Profile, bool, *AppError) {
	started := time.Now()
	valid := validSourceModel[Profile](username)
	type result struct {
		profile Profile
		err     *AppError
	}
	run := func(fetch func() (Profile, *AppError)) <-chan result {
		c := make(chan result, 1)
		go func() {
			p, err := fetch()
			c <- result{p, err}
		}()
		return c
	}
	rootC := run(func() (Profile, *AppError) {
		_, body, err := a.fetchViaProxy(ctx, loggedOutQuerySpec("profile", "PolarisLoggedOutDesktopWWWProfileRootContentQuery", profileRootDocID, map[string]any{"username": username}))
		if err != nil {
			return Profile{}, err
		}
		return parseProfileRoot(body)
	})
	postsC := run(func() (Profile, *AppError) {
		_, body, err := a.fetchViaProxy(ctx, loggedOutQuerySpec("profile_posts", "PolarisLoggedOutDesktopWWWProfilePostsTabContentQuery", profilePostsDocID, map[string]any{"first": profileGalleryMax, "username": username}))
		if err != nil {
			return Profile{}, err
		}
		return parseProfilePosts(body), nil
	})

	root := <-rootC
	if root.err != nil {
		if root.err.Final {
			return Profile{}, false, root.err
		}
		embed, err := valid(a.fetchProfileEmbed(ctx, username))
		if err != nil {
			return Profile{}, false, preferredError(root.err, err)
		}
		return embed, false, nil
	}
	p := root.profile
	wait := time.NewTimer(max(profilePostsWait-time.Since(started), 0))
	defer wait.Stop()
	select {
	case posts := <-postsC:
		if posts.err == nil {
			p.RecentMedia = posts.profile.RecentMedia
		}
	case <-wait.C:
	case <-ctx.Done():
	}
	p, err := valid(p, nil)
	return p, true, err
}

func parseProfileRoot(body string) (Profile, *AppError) {
	u := gjson.Get(body, "data.xig_user_by_username")
	if !present(u) {
		// null: no such, renamed or age-restricted account; a missing key is drift.
		if u.Exists() {
			notFound := igErr(http.StatusNotFound, errorCodeNotFound, "user not found")
			notFound.Final = true
			return Profile{}, notFound
		}
		return Profile{}, igErr(http.StatusBadGateway, errorCodeUpstream, "profile response had no user")
	}
	p := Profile{
		Username:      u.Get("username").String(),
		FullName:      u.Get("full_name").String(),
		Biography:     u.Get("biography").String(),
		ProfilePic:    u.Get("profile_pic_url").String(),
		FollowerCount: uintOf(u, "follower_count"),
		MediaCount:    unknownCount,
		IsPrivate:     u.Get("is_private").Bool(),
		IsVerified:    u.Get("is_verified").Type == gjson.True,
	}
	if p.Username == "" {
		return Profile{}, igErr(http.StatusBadGateway, errorCodeUpstream, "profile had no username")
	}
	return p, nil
}

// parseProfilePosts reads PostsTab's thumbnails (no sizes).
func parseProfilePosts(body string) Profile {
	var p Profile
	gjson.Get(body, "data.xig_user_by_username.polaris_ordered_timeline_connection.edges").ForEach(func(_, e gjson.Result) bool {
		if thumb := e.Get("node.display_uri").String(); thumb != "" {
			p.RecentMedia = append(p.RecentMedia, ProfileMedia{Thumbnail: thumb})
		}
		return len(p.RecentMedia) < profileGalleryMax
	})
	return p
}

func profileCDNURLs(p Profile) []string {
	urls := []string{p.ProfilePic}
	for _, m := range p.RecentMedia {
		urls = append(urls, m.Thumbnail)
	}
	return urls
}

func (a *App) getProfile(ctx context.Context, username string, meta *fetchMeta) (Profile, *AppError) {
	if !validUsername(username) {
		return Profile{}, igErr(404, errorCodeNotFound, "invalid username")
	}
	username = strings.ToLower(username)
	return a.profiles.get(ctx, username, meta, func(fetchCtx context.Context) (Profile, time.Duration, bool, *AppError) {
		p, persist, err := a.fetchProfile(fetchCtx, username)
		return p, cacheTTLFromURLs(profileCDNURLs(p)...), persist, err
	})
}
