package main

import (
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/tidwall/gjson"
)

type Profile struct {
	Username       string
	FullName       string
	Biography      string
	ProfilePic     string
	FollowerCount  int
	FollowingCount int
	MediaCount     int
	IsPrivate      bool
	Recent         []string
}

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`)

func validUsername(s string) bool { return usernameRE.MatchString(s) }

func webProfileSpec(username string) gqlSpec {
	return gqlSpec{
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
	for _, e := range u.Get("edge_owner_to_timeline_media.edges").Array() {
		img := bestProfileImage(e.Get("node"))
		if img == "" {
			continue
		}
		p.Recent = append(p.Recent, normalizeCDNHost(img))
		if len(p.Recent) >= profileGalleryMax {
			break
		}
	}
	return p, nil
}

func bestProfileImage(node gjson.Result) string {
	if u := bestCandidateURL(node.Get("thumbnail_resources")); u != "" {
		return u
	}
	for _, k := range []string{"thumbnail_src", "display_url"} {
		if u := node.Get(k).String(); u != "" {
			return u
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type profileEntry struct {
	profile   Profile
	err       *AppError
	expiresAt time.Time
}

type profileCall struct {
	done  chan struct{}
	entry *profileEntry
	err   *AppError
}

func (a *App) getProfile(username string, meta *fetchMeta) (Profile, *AppError) {
	if !validUsername(username) {
		return Profile{}, igErr(404, reasonNotFound, "invalid username")
	}

	a.profileMu.Lock()
	if e, ok := a.profiles[username]; ok && e.expiresAt.After(time.Now()) {
		a.profileMu.Unlock()
		return e.profile, e.err
	}
	a.profileMu.Unlock()

	a.profileFlightMu.Lock()
	if call, ok := a.profileFlight[username]; ok {
		a.profileFlightMu.Unlock()
		<-call.done
		if call.entry != nil {
			return call.entry.profile, call.err
		}
		return Profile{}, call.err
	}
	call := &profileCall{done: make(chan struct{})}
	a.profileFlight[username] = call
	a.profileFlightMu.Unlock()

	if meta != nil {
		meta.fetched = true
	}

	profile, err := a.fetchProfile(username)
	if err == nil || !err.Ephemeral {
		ttl := profileCacheTTLSeconds
		if err != nil {
			ttl = errorCacheSeconds(err.Reason)
		}
		entry := &profileEntry{profile: profile, err: err, expiresAt: time.Now().Add(time.Duration(ttl) * time.Second)}
		a.storeProfile(username, entry)
		call.entry, call.err = entry, err
	} else {
		call.err = err
	}

	a.profileFlightMu.Lock()
	delete(a.profileFlight, username)
	a.profileFlightMu.Unlock()
	close(call.done)
	return profile, err
}

func (a *App) storeProfile(username string, entry *profileEntry) {
	a.profileMu.Lock()
	defer a.profileMu.Unlock()
	if _, exists := a.profiles[username]; !exists {
		a.profileOrder = append(a.profileOrder, username)
	}
	a.profiles[username] = entry
	for len(a.profiles) > maxCacheEntries && len(a.profileOrder) > 0 {
		oldest := a.profileOrder[0]
		a.profileOrder = a.profileOrder[1:]
		delete(a.profiles, oldest)
	}
}
