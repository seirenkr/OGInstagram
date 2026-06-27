package main

import (
	"strconv"
	"strings"
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

const profilePrivateNotice = "🔒 This profile is private."

func profileStatsLine(p Profile) string {
	return "📝 " + fmtCount(p.MediaCount) + " 👤 " + fmtCount(p.FollowerCount)
}

const profileGalleryMax = 4

func profileMediaURLs(p Profile) []string {
	if p.IsPrivate {
		return nil
	}
	return p.Recent
}

func profileBioHTML(p Profile) string {
	bio := normalizeCaption(p.Biography)
	if bio == "" {
		return ""
	}
	return "<p>" + strings.ReplaceAll(htmlEscape(bio), "\n", "<br>") + "</p>"
}

func profileErrorCard(reason string) (title, desc string) {
	if isTransient(reason) {
		return "Temporarily unavailable", "Couldn't load this profile right now. Please try again in a moment."
	}
	return "Account unavailable", "This account isn't available - it may not exist, be deactivated, or the username is incorrect."
}

func (a *App) buildProfileEmbedHTML(baseURL, ua string, p Profile, gallery bool) string {
	origin := profileURL(p.Username)
	title := profileDisplayName(p) + " (@" + p.Username + ")"

	description := ""
	if !gallery {
		description = profileStatsLine(p)
		if bio := normalizeCaption(p.Biography); bio != "" {
			description += "\n\n" + bio
		}
		if p.IsPrivate {
			description += "\n\n" + profilePrivateNotice
		}
	}

	image := p.ProfilePic

	h := []string{
		`<meta charset="utf-8">`,
		`<link rel="canonical" href="` + attr(origin) + `">`,
		`<meta property="og:url" content="` + attr(origin) + `">`,
		`<meta property="og:type" content="profile">`,
		`<meta property="profile:username" content="` + attr(p.Username) + `">`,
		`<meta property="og:site_name" content="` + attr(a.cfg.BrandName) + `">`,
		`<meta property="og:title" content="` + attr(title) + `">`,
		`<meta property="twitter:title" content="` + attr(title) + `">`,
		`<meta property="twitter:site" content="@` + attr(p.Username) + `">`,
		`<meta property="twitter:creator" content="@` + attr(p.Username) + `">`,
		`<meta property="theme-color" content="` + attr(a.cfg.BrandColor) + `">`,
		`<meta property="twitter:card" content="summary">`,
		`<meta property="og:image" content="` + attr(image) + `">`,
		`<meta property="twitter:image" content="` + attr(image) + `">`,
	}
	if description != "" {
		h = append(h,
			`<meta name="description" content="`+attr(description)+`">`,
			`<meta property="og:description" content="`+attr(description)+`">`,
			`<meta property="twitter:description" content="`+attr(description)+`">`,
		)
	}
	h = append(h, a.faviconLinks(baseURL)...)
	if isDiscordBot(ua) {
		activityHref := baseURL + "/users/" + pathEscape(p.Username) + "/statuses/" + pathEscape(profileActivityCode(p.Username))
		h = append(h, `<link href="`+attr(activityHref)+`" rel="alternate" type="application/activity+json">`)
	}

	return compactHTML(`<!DOCTYPE html><html lang="en"><head>` + strings.Join(h, "") + `</head><body></body></html>`)
}

func profileAccountMap(p Profile) map[string]any {
	url := profileURL(p.Username)
	var avatar any
	if p.ProfilePic != "" {
		avatar = p.ProfilePic
	}
	var header any
	if thumbs := profileMediaURLs(p); len(thumbs) > 0 {
		header = thumbs[0]
	}
	return map[string]any{
		"id":              p.Username,
		"username":        p.Username,
		"acct":            p.Username,
		"display_name":    profileDisplayName(p),
		"note":            profileBioHTML(p),
		"url":             url,
		"uri":             url,
		"avatar":          avatar,
		"avatar_static":   avatar,
		"header":          header,
		"header_static":   header,
		"followers_count": p.FollowerCount,
		"following_count": p.FollowingCount,
		"statuses_count":  p.MediaCount,
		"locked":          p.IsPrivate,
		"bot":             false,
		"discoverable":    true,
		"group":           false,
		"created_at":      isoTime(epochUTC()),
		"last_status_at":  nil,
		"emojis":          []any{},
		"fields":          []any{},
	}
}

func (a *App) buildProfileAccount(p Profile) []byte {
	return jsonBytes(profileAccountMap(p))
}

func (a *App) buildProfileStatus(statusID string, p Profile) []byte {
	url := profileURL(p.Username)
	content := "<p><b>" + htmlEscape(profileStatsLine(p)) + "</b></p>" + profileBioHTML(p)
	if p.IsPrivate {
		content += "<p>" + htmlEscape(profilePrivateNotice) + "</p>"
	}

	thumbs := profileMediaURLs(p)
	media := make([]any, 0, len(thumbs))
	for i, t := range thumbs {
		media = append(media, map[string]any{
			"id":          strconv.Itoa(i + 1),
			"type":        "image",
			"url":         t,
			"preview_url": t,
			"remote_url":  nil,
			"description": nil,
			"meta":        map[string]any{"original": map[string]any{"aspect": 1.0}},
		})
	}

	return jsonBytes(map[string]any{
		"id":                     statusID,
		"url":                    url,
		"uri":                    url,
		"created_at":             nil,
		"edited_at":              nil,
		"content":                content,
		"spoiler_text":           "",
		"visibility":             "public",
		"language":               "en",
		"in_reply_to_id":         nil,
		"in_reply_to_account_id": nil,
		"reblog":                 nil,
		"media_attachments":      media,
		"account":                profileAccountMap(p),
		"application":            map[string]any{"name": "Instagram", "website": nil},
		"mentions":               []any{},
		"tags":                   []any{},
		"emojis":                 []any{},
		"card":                   nil,
		"poll":                   nil,
	})
}
