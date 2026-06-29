package main

import (
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/tidwall/gjson"
)

const (
	profilePrivateNotice = "🔒 This profile is private."

	profileGalleryMax = 6
)

func profileURL(username string) string {
	return instagramOrigin + "/" + pathEscape(username) + "/"
}

func profileDisplayName(p Profile) string {
	if p.FullName != "" {
		return p.FullName
	}
	return p.Username
}

func profileStatsLine(p Profile) string {
	return "📝 " + fmtCount(p.MediaCount) + " 👤 " + fmtCount(p.FollowerCount)
}

func profileBioHTML(p Profile) string {
	bio := normalizeCaption(p.Biography)
	if bio == "" {
		return ""
	}
	return "<p>" + captionHTML(bio) + "</p>"
}

func profileErrorCard(reason string) (title, desc string) {
	if isTransient(reason) {
		return "Temporarily unavailable", "Couldn't load this profile right now. Please try again in a moment."
	}
	return "Account unavailable", "This account isn't available - it may not exist, be deactivated, or the username is incorrect."
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

func webProfileSpec(username string) gqlSpec {
	return gqlSpec{
		name:   "profile",
		target: username,
		method: http.MethodGet,
		url:    instagramOrigin + "/api/v1/users/web_profile_info/?username=" + url.QueryEscape(username),
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

func (a *App) fetchProfile(username string) (Profile, *AppError) {
	body, err := a.raceFetch(webProfileSpec(username))
	if err != nil {
		return Profile{}, err
	}
	return parseProfile(body)
}

func parseProfile(body string) (Profile, *AppError) {
	u := gjson.Get(body, "data.user")
	if !present(u) {
		return Profile{}, igErr(404, reasonNotFound, "user not found")
	}
	p := Profile{
		Username:       u.Get("username").String(),
		UserID:         u.Get("id").String(),
		FullName:       u.Get("full_name").String(),
		Biography:      u.Get("biography").String(),
		ProfilePic:     normalizeCDNHost(firstNonEmpty(u.Get("profile_pic_url_hd").String(), u.Get("profile_pic_url").String())),
		FollowerCount:  uintOf(u, "edge_followed_by.count"),
		FollowingCount: uintOf(u, "edge_follow.count"),
		MediaCount:     uintOf(u, "edge_owner_to_timeline_media.count"),
		IsPrivate:      u.Get("is_private").Bool(),
	}
	if p.Username == "" {
		return Profile{}, igErr(404, reasonMediaNotFound, "profile had no username")
	}
	u.Get("edge_owner_to_timeline_media.edges").ForEach(func(_, e gjson.Result) bool {
		n := e.Get("node")
		thumb := normalizeCDNHost(firstNonEmpty(n.Get("thumbnail_src").String(), n.Get("display_url").String()))
		if thumb != "" {
			var takenAt time.Time
			if ts := n.Get("taken_at_timestamp").Int(); ts > 0 {
				takenAt = time.Unix(ts, 0).UTC()
			}
			p.RecentMedia = append(p.RecentMedia, ProfileMedia{
				ID:        firstNonEmpty(n.Get("pk").String(), n.Get("id").String()),
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *App) getProfile(username string, meta *fetchMeta) (Profile, *AppError) {
	if !validUsername(username) {
		return Profile{}, igErr(404, reasonNotFound, "invalid username")
	}
	return a.profiles.get(username, meta, func() (Profile, time.Duration, *AppError) {
		p, err := a.fetchProfile(username)
		return p, profileCacheTTLSeconds * time.Second, err
	})
}
