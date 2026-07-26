package main

import (
	"cmp"
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

func profileStatsLine(p Profile) string {
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
	Username       string
	UserID         string
	FullName       string
	Biography      string
	ProfilePic     string
	FollowerCount  int
	FollowingCount int
	MediaCount     int
	IsPrivate      bool
	RecentMedia    []ProfileMedia
}

type ProfileMedia struct {
	ID        string
	Thumbnail string
	Width     int
	Height    int
	TakenAt   time.Time
}

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`)

func validUsername(s string) bool { return usernameRE.MatchString(s) }

func webProfileSpec(username string) fetchSpec {
	return fetchSpec{
		operation: "profile",
		subject:   "Instagram",
		interpret: instagramJSON,
		method:    http.MethodGet,
		url:       instagramOrigin + "/api/v1/users/web_profile_info/?username=" + url.QueryEscape(username),
		headers: map[string]string{
			"User-Agent":                  instagramWebUA,
			"Accept":                      "*/*",
			"Accept-Language":             "en-US,en;q=0.9",
			"X-IG-App-ID":                 instagramAppID,
			"X-Asbd-Id":                   instagramAsbdID,
			"X-Requested-With":            "XMLHttpRequest",
			"Sec-Ch-Prefers-Color-Scheme": "dark",
			"Sec-Ch-Ua-Platform":          `"Linux"`,
			"Sec-Ch-Ua-Mobile":            "?0",
			"Sec-Fetch-Site":              "same-origin",
			"Sec-Fetch-Mode":              "cors",
			"Sec-Fetch-Dest":              "empty",
			"Referer":                     instagramOrigin + "/",
		},
	}
}

func (a *App) fetchProfile(ctx context.Context, username string) (Profile, *AppError) {
	return stagedFetch(ctx,
		stagedSource[Profile]{fetch: func(ctx context.Context) (Profile, *AppError) {
			_, body, err := a.fetchViaProxy(ctx, webProfileSpec(username))
			if err != nil {
				return Profile{}, err
			}
			return parseProfile(body)
		}},
		stagedSource[Profile]{after: profileHedgeDelay, fetch: func(ctx context.Context) (Profile, *AppError) {
			return a.fetchProfileEmbed(ctx, username)
		}},
	)
}

func parseProfile(body string) (Profile, *AppError) {
	u := gjson.Get(body, "data.user")
	if !present(u) {
		return Profile{}, igErr(404, errorCodeNotFound, "user not found")
	}
	p := Profile{
		Username:       u.Get("username").String(),
		UserID:         u.Get("id").String(),
		FullName:       u.Get("full_name").String(),
		Biography:      u.Get("biography").String(),
		ProfilePic:     normalizeCDNHost(cmp.Or(u.Get("profile_pic_url_hd").String(), u.Get("profile_pic_url").String())),
		FollowerCount:  uintOf(u, "edge_followed_by.count"),
		FollowingCount: uintOf(u, "edge_follow.count"),
		MediaCount:     uintOf(u, "edge_owner_to_timeline_media.count"),
		IsPrivate:      u.Get("is_private").Bool(),
	}
	if p.Username == "" {
		return Profile{}, igErr(404, errorCodeMediaNotFound, "profile had no username")
	}
	u.Get("edge_owner_to_timeline_media.edges").ForEach(func(_, e gjson.Result) bool {
		n := e.Get("node")
		thumb := normalizeCDNHost(cmp.Or(n.Get("thumbnail_src").String(), n.Get("display_url").String()))
		if thumb != "" {
			var takenAt time.Time
			if ts := n.Get("taken_at_timestamp").Int(); ts > 0 {
				takenAt = time.Unix(ts, 0).UTC()
			}
			p.RecentMedia = append(p.RecentMedia, ProfileMedia{
				ID:        cmp.Or(n.Get("pk").String(), n.Get("id").String()),
				Thumbnail: thumb,
				Width:     int(n.Get("dimensions.width").Int()),
				Height:    int(n.Get("dimensions.height").Int()),
				TakenAt:   takenAt,
			})
		}
		return len(p.RecentMedia) < profileGalleryMax
	})
	return p, nil
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
	return a.profiles.get(ctx, username, meta, func(fetchCtx context.Context) (Profile, time.Duration, *AppError) {
		p, err := a.fetchProfile(fetchCtx, username)
		return p, cacheTTLFromURLs(profileCDNURLs(p)...), err
	})
}
