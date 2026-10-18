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

func webProfileSpec(username string) fetchSpec {
	return fetchSpec{
		operation:  "profile",
		subject:    "Instagram",
		expectJSON: true,
		interpret:  instagramJSON,
		method:     http.MethodGet,
		url:        instagramOrigin + "/api/v1/users/web_profile_info/?username=" + url.QueryEscape(username),
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

func (a *App) fetchProfile(ctx context.Context, username string) (Profile, bool, *AppError) {
	return stagedFetch(ctx,
		stagedSource[Profile]{name: "profile_web", persist: true, fetch: func(ctx context.Context) (Profile, *AppError) {
			_, body, err := a.fetchViaProxy(ctx, webProfileSpec(username))
			if err != nil {
				return Profile{}, err
			}
			return validSourceModel[Profile](username)(parseProfile(body))
		}},
		stagedSource[Profile]{name: "profile_embed", after: profileHedgeDelay, fetch: func(ctx context.Context) (Profile, *AppError) {
			return validSourceModel[Profile](username)(a.fetchProfileEmbed(ctx, username))
		}},
	)
}

func parseProfile(body string) (Profile, *AppError) {
	u := gjson.Get(body, "data.user")
	if !present(u) {
		// Only an explicit "user": null means the account is gone; any other shape is drift.
		if u.Exists() {
			return Profile{}, igErr(404, errorCodeNotFound, "user not found")
		}
		return Profile{}, igErr(502, errorCodeUpstream, "profile response had no user")
	}
	p := Profile{
		Username:      u.Get("username").String(),
		FullName:      u.Get("full_name").String(),
		Biography:     u.Get("biography").String(),
		ProfilePic:    cmp.Or(u.Get("profile_pic_url_hd").String(), u.Get("profile_pic_url").String()),
		FollowerCount: uintOf(u, "edge_followed_by.count"),
		MediaCount:    uintOf(u, "edge_owner_to_timeline_media.count"),
		IsPrivate:     u.Get("is_private").Bool(),
		IsVerified:    u.Get("is_verified").Type == gjson.True,
	}
	if p.Username == "" {
		return Profile{}, igErr(502, errorCodeUpstream, "profile had no username")
	}
	u.Get("edge_owner_to_timeline_media.edges").ForEach(func(_, e gjson.Result) bool {
		n := e.Get("node")
		if thumb := bestGraphImageURL(n); thumb != "" {
			p.RecentMedia = append(p.RecentMedia, ProfileMedia{
				Thumbnail: thumb,
				Width:     uintOf(n, "dimensions.width"),
				Height:    uintOf(n, "dimensions.height"),
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
	return a.profiles.get(ctx, username, meta, func(fetchCtx context.Context) (Profile, time.Duration, bool, *AppError) {
		p, persist, err := a.fetchProfile(fetchCtx, username)
		return p, cacheTTLFromURLs(profileCDNURLs(p)...), persist, err
	})
}
