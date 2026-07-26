package main

import (
	"html"
	"strconv"
	"strings"
)

func videoOGTags(mediaHref string, att Attachment) []string {
	w, h := videoDisplaySize(att)
	tags := []string{
		`<meta property="og:video" content="` + html.EscapeString(mediaHref) + `">`,
		`<meta property="og:video:secure_url" content="` + html.EscapeString(mediaHref) + `">`,
		`<meta property="og:video:type" content="video/mp4">`,
	}
	return append(tags, dimensionTags("property", "og:video", w, h)...)
}

func dimensionTags(attr, prefix string, w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	return []string{
		`<meta ` + attr + `="` + prefix + `:width" content="` + strconv.Itoa(w) + `">`,
		`<meta ` + attr + `="` + prefix + `:height" content="` + strconv.Itoa(h) + `">`,
	}
}

func displayTitle(name, username string) string {
	if name == "" {
		name = username
	}
	return name + " (@" + username + ")"
}

func (a *App) commonHead(baseURL, originURL, username, title, description, image, card, activityHref string) []string {
	h := []string{
		`<meta charset="utf-8">`,
		`<link rel="canonical" href="` + html.EscapeString(originURL) + `">`,
		`<meta property="og:url" content="` + html.EscapeString(originURL) + `">`,
		`<meta property="og:locale" content="en_US">`,
		`<meta property="og:site_name" content="` + html.EscapeString(brandName) + `">`,
		`<meta property="og:title" content="` + html.EscapeString(title) + `">`,
		`<meta name="twitter:title" content="` + html.EscapeString(title) + `">`,
		`<meta name="theme-color" content="` + html.EscapeString(brandColor) + `">`,
		`<meta name="twitter:card" content="` + card + `">`,
		`<meta name="description" content="` + html.EscapeString(description) + `">`,
		`<meta property="og:description" content="` + html.EscapeString(description) + `">`,
		`<meta name="twitter:description" content="` + html.EscapeString(description) + `">`,
		`<link href="` + html.EscapeString(baseURL) + `/favicon-64.png" rel="icon" sizes="64x64" type="image/png">`,
	}
	if image != "" {
		h = append(h,
			`<meta property="og:image" content="`+html.EscapeString(image)+`">`,
			`<meta property="og:image:secure_url" content="`+html.EscapeString(image)+`">`,
			`<meta name="twitter:image" content="`+html.EscapeString(image)+`">`,
		)
	}
	if username != "" {
		h = append(h, `<meta name="twitter:creator" content="@`+html.EscapeString(username)+`">`)
	}
	if activityHref != "" {
		h = append(h, `<link href="`+html.EscapeString(activityHref)+`" rel="alternate" type="application/activity+json">`)
	}
	return h
}

func (a *App) buildEmbedHTML(baseURL string, post Post, postType string, mediaIndex int, specified, gallery bool) string {
	selectedIndex := mediaIndexFor(post, mediaIndex)
	first := post.Attachments[selectedIndex]
	originURL := instagramPostURL(postType, post.Shortcode, selectedIndex, specified)
	title := displayTitle(post.FullName, post.Username)
	imageAlt := truncateFlat(post.Caption, 420)
	useActivity := post.Username != ""

	indicator := singleAttachmentIndicator(post, selectedIndex)
	if useActivity {
		indicator = selectActivityAttachments(post, mediaIndex, specified).indicator
	}
	description := postDescription(post.Caption, withIndicator(indicator, post.StatsLine))
	if gallery {
		imageAlt = ""
		description = ""
	}

	mediaHref := a.offloadURL(baseURL, post.Shortcode, selectedIndex, false)
	thumbnailHref := a.offloadURL(baseURL, post.Shortcode, selectedIndex, true)
	exposeVideo := first.Kind == "video" && first.URL != ""

	activityHref := ""
	if useActivity {
		activityHref = statusURL(baseURL, post.Username, postType, post.Shortcode, selectedIndex, specified, gallery)
	}

	h := a.commonHead(baseURL, originURL, post.Username, title, description, thumbnailHref, "summary_large_image", activityHref)
	h = append(h,
		`<meta property="og:type" content="article">`,
		`<link rel="apple-touch-icon" href="`+html.EscapeString(a.postAvatarURL(baseURL, post))+`">`,
		`<meta property="article:author" content="`+instagramOrigin+"/"+html.EscapeString(post.Username)+`/">`,
	)
	h = append(h, dimensionTags("property", "og:image", first.Width, first.Height)...)
	if published := isoTime(post.CreatedAt); published != "" {
		h = append(h, `<meta property="article:published_time" content="`+html.EscapeString(published)+`">`)
	}
	if imageAlt != "" {
		h = append(h,
			`<meta name="twitter:image:alt" content="`+html.EscapeString(imageAlt)+`">`,
			`<meta property="og:image:alt" content="`+html.EscapeString(imageAlt)+`">`,
		)
	}
	if exposeVideo {
		h = append(h, videoOGTags(mediaHref, first)...)
	}

	return embedDocument(h)
}

func embedDocument(head []string) string {
	return `<!DOCTYPE html><html lang="en"><head>` + embedBanner + strings.Join(head, "") + `</head><body></body></html>`
}

const embedBanner = `<!--

 _____ _____ _____         _
|     |   __|     |___ ___| |_ ___ ___ ___ ___ _____
|  |  |  |  |-   -|   |_ -|  _| .'| . |  _| .'|     |
|_____|_____|_____|_|_|___|_| |__,|_  |_| |__,|_|_|_|
                                  |___|

 Instagram embed proxy for Discord, Telegram, and anything that supports
 Open Graph Protocol or ActivityPub — with rich previews: media, caption, and stats.

-->`

func (a *App) buildProfileEmbedHTML(baseURL string, p Profile, gallery bool) string {
	origin := profileURL(p.Username)
	title := displayTitle(p.FullName, p.Username)

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

	h := a.commonHead(baseURL, origin, p.Username, title, description, a.profileAvatarURL(baseURL, p), "summary", profileStatusURL(baseURL, p.Username))
	h = append(h,
		`<meta property="og:type" content="profile">`,
		`<meta property="profile:username" content="`+html.EscapeString(p.Username)+`">`,
	)
	return embedDocument(h)
}

func (a *App) buildStatusEmbedHTML(baseURL, originURL, title, description string) string {
	h := a.commonHead(baseURL, originURL, "", title, description, "", "summary", "")
	h = append(h, `<meta property="og:type" content="article">`)
	return embedDocument(h)
}
