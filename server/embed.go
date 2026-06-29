package main

import (
	"strconv"
	"strings"
)

func isTelegramBot(ua string) bool { return strings.Contains(strings.ToLower(ua), "telegrambot") }

func (a *App) faviconLinks(baseURL string) []string {
	return []string{
		`<link href="` + baseURL + `/favicon-64.png" rel="icon" sizes="64x64" type="image/png">`,
		`<link href="` + baseURL + `/favicon-48.png" rel="icon" sizes="48x48" type="image/png">`,
		`<link href="` + baseURL + `/favicon-32.png" rel="icon" sizes="32x32" type="image/png">`,
		`<link href="` + baseURL + `/favicon-24.png" rel="icon" sizes="24x24" type="image/png">`,
		`<link href="` + baseURL + `/favicon-16.png" rel="icon" sizes="16x16" type="image/png">`,
	}
}

func (a *App) commonHead(baseURL, originURL, username, title, description, image, card, activityHref string) []string {
	h := []string{
		`<meta charset="utf-8">`,
		`<link rel="canonical" href="` + attr(originURL) + `">`,
		`<meta property="og:url" content="` + attr(originURL) + `">`,
		`<meta property="og:locale" content="en_US">`,
		`<meta property="og:site_name" content="` + attr(a.cfg.BrandName) + `">`,
		`<meta property="og:title" content="` + attr(title) + `">`,
		`<meta name="twitter:title" content="` + attr(title) + `">`,
		`<meta name="twitter:creator" content="@` + attr(username) + `">`,
		`<meta name="theme-color" content="` + attr(a.cfg.BrandColor) + `">`,
		`<meta name="twitter:card" content="` + card + `">`,
		`<meta property="og:image" content="` + attr(image) + `">`,
		`<meta property="og:image:secure_url" content="` + attr(image) + `">`,
		`<meta name="twitter:image" content="` + attr(image) + `">`,
		`<meta name="description" content="` + attr(description) + `">`,
		`<meta property="og:description" content="` + attr(description) + `">`,
		`<meta name="twitter:description" content="` + attr(description) + `">`,
	}
	h = append(h, a.faviconLinks(baseURL)...)
	if activityHref != "" {
		h = append(h, `<link href="`+attr(activityHref)+`" rel="alternate" type="application/activity+json">`)
	}
	return h
}

func (a *App) buildEmbedHTML(baseURL, ua string, post Post, postType string, mediaIndex int, specified, gallery bool) string {
	selectedIndex := mediaIndexFor(post, mediaIndex)
	first := post.Attachments[selectedIndex]
	originURL := instagramURLForSelection(postType, post.Shortcode, selectedIndex, specified)
	title := post.FullName + " (@" + post.Username + ")"
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

	mediaHref := offloadURL(baseURL, post.Shortcode, selectedIndex, false)
	thumbnailHref := offloadURL(baseURL, post.Shortcode, selectedIndex, true)
	exposeVideo := first.Kind == "video" && !first.OversizedInline

	// og:type stays "article" even for video; og:type=video.other makes Discord hide the caption.
	card, ogType := "summary_large_image", "article"

	activityHref := ""
	if useActivity {
		activityHref = statusURL(baseURL, post.Username, postType, post.Shortcode, selectedIndex, specified, gallery)
	}

	h := a.commonHead(baseURL, originURL, post.Username, title, description, thumbnailHref, card, activityHref)
	h = append(h,
		`<meta property="og:type" content="`+ogType+`">`,
		`<link rel="apple-touch-icon" href="`+attr(post.ProfilePic)+`">`,
		`<meta property="article:author" content="`+instagramOrigin+"/"+attr(post.Username)+`/">`,
		`<meta property="article:published_time" content="`+attr(isoTime(post.CreatedAt))+`">`,
		`<meta name="twitter:image:width" content="`+strconv.Itoa(first.Width)+`">`,
		`<meta name="twitter:image:height" content="`+strconv.Itoa(first.Height)+`">`,
		`<meta property="og:image:width" content="`+strconv.Itoa(first.Width)+`">`,
		`<meta property="og:image:height" content="`+strconv.Itoa(first.Height)+`">`,
	)
	if imageAlt != "" {
		h = append(h,
			`<meta name="twitter:image:alt" content="`+attr(imageAlt)+`">`,
			`<meta property="og:image:alt" content="`+attr(imageAlt)+`">`,
		)
	}
	if !isTelegramBot(ua) {
		h = append(h, `<meta http-equiv="refresh" content="0;url=`+attr(originURL)+`">`)
	}
	if exposeVideo {
		vidW, vidH := videoDisplaySize(first)
		h = append(h,
			`<meta property="og:video" content="`+attr(mediaHref)+`">`,
			`<meta property="og:video:secure_url" content="`+attr(mediaHref)+`">`,
			`<meta property="og:video:type" content="video/mp4">`,
			`<meta property="og:video:width" content="`+strconv.Itoa(vidW)+`">`,
			`<meta property="og:video:height" content="`+strconv.Itoa(vidH)+`">`,
		)
	}

	return compactHTML(`<!DOCTYPE html>` + embedBanner + `<html lang="en"><head>` + strings.Join(h, "") + `</head><body></body></html>`)
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

	h := a.commonHead(baseURL, origin, p.Username, title, description, p.ProfilePic, "summary", profileStatusURL(baseURL, p.Username))
	h = append(h,
		`<meta property="og:type" content="profile">`,
		`<meta property="profile:username" content="`+attr(p.Username)+`">`,
	)
	return compactHTML(`<!DOCTYPE html>` + embedBanner + `<html lang="en"><head>` + strings.Join(h, "") + `</head><body></body></html>`)
}

func postErrorCard(reason string) (title, desc string) {
	if isTransient(reason) {
		return "Temporarily unavailable", "Couldn't load this post right now. Please try again in a moment."
	}
	return "Post unavailable", "This post isn't available - it may be deleted, set to private, or the link is incorrect."
}

func (a *App) buildStatusEmbedHTML(baseURL, originURL, title, description string) string {
	h := []string{
		`<meta charset="utf-8">`,
		`<link rel="canonical" href="` + attr(originURL) + `">`,
		`<meta property="og:url" content="` + attr(originURL) + `">`,
		`<meta property="og:type" content="article">`,
		`<meta property="og:site_name" content="` + attr(a.cfg.BrandName) + `">`,
		`<meta property="og:title" content="` + attr(title) + `">`,
		`<meta name="twitter:title" content="` + attr(title) + `">`,
		`<meta property="og:image" content="` + attr(baseURL+"/favicon-192.png") + `">`,
		`<meta name="twitter:image" content="` + attr(baseURL+"/favicon-192.png") + `">`,
		`<meta name="description" content="` + attr(description) + `">`,
		`<meta property="og:description" content="` + attr(description) + `">`,
		`<meta name="twitter:description" content="` + attr(description) + `">`,
		`<meta name="twitter:card" content="summary">`,
		`<meta name="theme-color" content="` + attr(a.cfg.BrandColor) + `">`,
	}
	h = append(h, a.faviconLinks(baseURL)...)
	return compactHTML(`<!DOCTYPE html>` + embedBanner + `<html lang="en"><head>` + strings.Join(h, "") + `</head><body></body></html>`)
}
