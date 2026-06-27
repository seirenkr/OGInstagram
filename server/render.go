package main

import (
	"encoding/json"
	"math/big"
	"net/url"
	"strconv"
	"strings"
)

func isTelegramBot(ua string) bool { return strings.Contains(strings.ToLower(ua), "telegrambot") }
func isDiscordBot(ua string) bool  { return strings.Contains(strings.ToLower(ua), "discordbot") }

func (a *App) faviconLinks(baseURL string) []string {
	sizes := []string{"64", "48", "32", "24", "16"}
	links := make([]string, 0, len(sizes))
	for _, s := range sizes {
		links = append(links, `<link href="`+baseURL+a.assets.faviconPath(s)+`" rel="icon" sizes="`+s+`x`+s+`" type="image/png">`)
	}
	return links
}

func (a *App) buildEmbedHTML(baseURL, ua string, post Post, postType string, mediaIndex int, specified, gallery bool) string {
	selectedIndex := mediaIndexFor(post, mediaIndex)
	first := post.Attachments[selectedIndex]
	originURL := instagramURLForSelection(postType, post.Shortcode, selectedIndex, specified)
	title := post.FullName + " (@" + post.Username + ")"
	imageAlt := truncateFlat(post.Caption, 420)
	if gallery {
		imageAlt = ""
	}
	isTelegram := isTelegramBot(ua)
	isDiscord := isDiscordBot(ua)

	exposeVideo := first.Kind == "video" && !first.OversizedInline
	useActivity := isDiscord && post.Username != ""

	var indicator string
	if useActivity {
		indicator = selectActivityAttachments(post, mediaIndex, specified).indicator
	} else {
		indicator = singleAttachmentIndicator(post, selectedIndex)
	}
	statsLine := withIndicator(indicator, post.StatsLine)
	description := postDescription(post.Caption, statsLine)
	if gallery {
		description = ""
	}

	mediaHref := offloadURL(baseURL, post.Shortcode, selectedIndex, false)
	thumbnailHref := offloadURL(baseURL, post.Shortcode, selectedIndex, true)

	ov := url.Values{}
	ov.Set("url", originURL)
	ov.Set("shortcode", post.Shortcode)
	ov.Set("text", truncateFlat(statsLine, 255))
	if gallery {
		ov.Set("text", "")
		ov.Set("__gallery", "1")
	}
	ov.Set("status", post.Shortcode)
	ov.Set("post_type", postType)
	if specified {
		ov.Set("img_index", strconv.Itoa(selectedIndex+1))
	}
	oembedHref := baseURL + "/owoembed?" + ov.Encode()

	h := []string{
		`<meta charset="utf-8">`,
		`<link rel="canonical" href="` + attr(originURL) + `">`,
		`<meta property="og:url" content="` + attr(originURL) + `">`,
		`<meta property="twitter:site" content="@` + attr(post.Username) + `">`,
		`<meta property="twitter:creator" content="@` + attr(post.Username) + `">`,
		`<meta property="theme-color" content="` + attr(a.cfg.BrandColor) + `">`,
		`<meta property="twitter:title" content="` + attr(title) + `">`,
		`<link rel="apple-touch-icon" href="` + attr(post.ProfilePic) + `">`,
		`<meta property="article:author" content="` + instagramOrigin + "/" + attr(post.Username) + `/">`,
		`<meta property="article:published_time" content="` + attr(isoTime(post.CreatedAt)) + `">`,
	}
	h = append(h, a.faviconLinks(baseURL)...)

	if !isTelegram {
		h = append(h, `<meta http-equiv="refresh" content="0;url=`+attr(originURL)+`">`)
	}
	twitterCard := "summary_large_image"
	if exposeVideo {
		twitterCard = "player"
	}
	ogType := "article"
	if exposeVideo {
		ogType = "video.other"
	}
	h = append(h,
		`<meta property="twitter:card" content="`+twitterCard+`">`,
		`<meta property="og:title" content="`+attr(title)+`">`,
		`<meta name="description" content="`+attr(description)+`">`,
		`<meta property="og:description" content="`+attr(description)+`">`,
		`<meta property="twitter:description" content="`+attr(description)+`">`,
		`<meta property="og:site_name" content="`+attr(a.cfg.BrandName)+`">`,
		`<meta property="og:type" content="`+ogType+`">`,
		`<meta property="twitter:image" content="`+attr(thumbnailHref)+`">`,
		`<meta property="og:image" content="`+attr(thumbnailHref)+`">`,
		`<meta property="twitter:image:width" content="`+strconv.Itoa(first.Width)+`">`,
		`<meta property="twitter:image:height" content="`+strconv.Itoa(first.Height)+`">`,
		`<meta property="og:image:width" content="`+strconv.Itoa(first.Width)+`">`,
		`<meta property="og:image:height" content="`+strconv.Itoa(first.Height)+`">`,
	)
	if imageAlt != "" {
		h = append(h,
			`<meta property="twitter:image:alt" content="`+attr(imageAlt)+`">`,
			`<meta property="og:image:alt" content="`+attr(imageAlt)+`">`,
		)
	}
	if exposeVideo {
		h = append(h,
			`<meta property="og:video" content="`+attr(mediaHref)+`">`,
			`<meta property="og:video:secure_url" content="`+attr(mediaHref)+`">`,
			`<meta property="og:video:type" content="video/mp4">`,
			`<meta property="og:video:width" content="`+strconv.Itoa(first.Width)+`">`,
			`<meta property="og:video:height" content="`+strconv.Itoa(first.Height)+`">`,
			`<meta property="twitter:player:width" content="`+strconv.Itoa(first.Width)+`">`,
			`<meta property="twitter:player:height" content="`+strconv.Itoa(first.Height)+`">`,
			`<meta property="twitter:player:stream" content="`+attr(mediaHref)+`">`,
			`<meta property="twitter:player:stream:content_type" content="video/mp4">`,
		)
	}
	h = append(h, `<link rel="alternate" href="`+attr(oembedHref)+`" type="application/json+oembed" title="`+attr(post.FullName)+`">`)
	if useActivity {
		activityCode := activityCodeFor(postType, post.Shortcode, selectedIndex, specified, gallery)
		activityHref := baseURL + "/users/" + pathEscape(post.Username) + "/statuses/" + pathEscape(activityCode)
		h = append(h, `<link href="`+attr(activityHref)+`" rel="alternate" type="application/activity+json">`)
	}

	return compactHTML(`<!DOCTYPE html><html lang="en"><!--

` + a.cfg.BrandName + `
A lightweight Open Graph and ActivityPub bridge for Instagram links.
--><head>` + strings.Join(h, "") + `</head><body></body></html>`)
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
		`<meta property="twitter:title" content="` + attr(title) + `">`,
		`<meta name="description" content="` + attr(description) + `">`,
		`<meta property="og:description" content="` + attr(description) + `">`,
		`<meta property="twitter:description" content="` + attr(description) + `">`,
		`<meta property="twitter:card" content="summary">`,
		`<meta property="theme-color" content="` + attr(a.cfg.BrandColor) + `">`,
	}
	h = append(h, a.faviconLinks(baseURL)...)
	return compactHTML(`<!DOCTYPE html><html lang="en"><head>` + strings.Join(h, "") + `</head><body></body></html>`)
}

func (a *App) buildOEmbed(baseURL string, post Post, postType string, mediaIndex int, specified bool) []byte {
	selectedIndex := mediaIndexFor(post, mediaIndex)
	att := post.Attachments[selectedIndex]
	return jsonBytes(map[string]any{
		"version":          "1.0",
		"type":             "rich",
		"author_name":      truncateFlat(post.FullName+" (@"+post.Username+")", 255),
		"author_url":       instagramURLForSelection(postType, post.Shortcode, selectedIndex, specified),
		"provider_name":    a.cfg.BrandName,
		"provider_url":     baseURL,
		"title":            truncateFlat(withIndicator(singleAttachmentIndicator(post, selectedIndex), post.StatsLine), 255),
		"thumbnail_url":    offloadURL(baseURL, post.Shortcode, selectedIndex, true),
		"thumbnail_width":  att.Width,
		"thumbnail_height": att.Height,
	})
}

func (a *App) buildRateLimitOEmbed(baseURL, postType, shortcode string, mediaIndex int, specified bool) []byte {
	authorURL := instagramOrigin
	if shortcode != "" {
		authorURL = instagramURLForSelection(postType, shortcode, mediaIndex, specified)
	}
	return jsonBytes(map[string]any{
		"version":       "1.0",
		"type":          "rich",
		"author_name":   rateLimitTitle,
		"author_url":    authorURL,
		"provider_name": a.cfg.BrandName,
		"provider_url":  baseURL,
		"title":         rateLimitDescription,
	})
}

func (a *App) buildOwOEmbed(baseURL string, q RequestQuery) []byte {
	provider := q.Provider
	if provider == "" {
		provider = a.cfg.BrandName
	}
	postType := normalizePostType(q.PostType)
	mediaIndex := mediaIndexFromQuery(q, -1)
	specified := querySpecified(q)
	status := q.Status
	if status == "" {
		status = q.Shortcode
	}
	statusURL := instagramOrigin
	if status != "" {
		statusURL = instagramURLForSelection(postType, status, mediaIndex, specified)
	}
	providerURL := statusURL
	if provider == a.cfg.BrandName {
		providerURL = baseURL
	}
	authorName := q.Text
	if authorName == "" {
		authorName = "Embed"
	}
	return jsonBytes(map[string]any{
		"version":       "1.0",
		"type":          "rich",
		"author_name":   authorName,
		"author_url":    statusURL,
		"provider_name": provider,
		"provider_url":  providerURL,
		"title":         "Embed",
	})
}

func (a *App) buildActivityStatus(baseURL, statusID string, post Post, postType string, mediaIndex int, specified, gallery bool) []byte {
	selectedIndex := mediaIndexFor(post, mediaIndex)
	postURL := instagramURLForSelection(postType, post.Shortcode, selectedIndex, specified)
	selection := selectActivityAttachments(post, mediaIndex, specified)
	caption := normalizeCaption(post.Caption)
	content := "<p><b>" + htmlEscape(withIndicator(selection.indicator, post.StatsLine)) + "</b></p>"
	if caption != "" {
		content += "<p>" + strings.ReplaceAll(htmlEscape(caption), "\n", "<br>") + "</p>"
	}
	if gallery {
		content = ""
	}
	authorURL := profileURL(post.Username)

	media := make([]any, 0, len(selection.items))
	for _, it := range selection.items {
		media = append(media, buildActivityMediaAttachment(baseURL, post, it.att, it.index))
	}

	return jsonBytes(map[string]any{
		"id":                     statusID,
		"url":                    postURL,
		"uri":                    postURL,
		"created_at":             isoTime(post.CreatedAt),
		"edited_at":              nil,
		"reblog":                 nil,
		"in_reply_to_id":         nil,
		"in_reply_to_account_id": nil,
		"content":                content,
		"spoiler_text":           "",
		"language":               "en",
		"visibility":             "public",
		"application":            map[string]any{"name": "Instagram", "website": nil},
		"media_attachments":      media,
		"account":                activityAccount(post, authorURL),
		"mentions":               []any{},
		"tags":                   []any{},
		"emojis":                 []any{},
		"card":                   nil,
		"poll":                   nil,
	})
}

func activityAccount(post Post, authorURL string) map[string]any {
	var avatar any
	if s := optionalString(post.ProfilePic); s != "" {
		avatar = s
	}
	return map[string]any{
		"id":               post.Username,
		"display_name":     post.FullName,
		"username":         post.Username,
		"acct":             post.Username,
		"url":              authorURL,
		"uri":              authorURL,
		"created_at":       isoTime(post.CreatedAt),
		"locked":           false,
		"bot":              false,
		"discoverable":     true,
		"indexable":        false,
		"group":            false,
		"avatar":           avatar,
		"avatar_static":    avatar,
		"followers_count":  0,
		"following_count":  0,
		"statuses_count":   0,
		"hide_collections": false,
		"noindex":          false,
		"emojis":           []any{},
		"roles":            []any{},
		"fields":           []any{},
	}
}

func (a *App) buildFallbackAccount(username string) []byte {
	safe := strings.TrimSpace(username)
	if safe == "" {
		safe = a.cfg.BrandName
	}
	accountURL := profileURL(safe)
	return jsonBytes(map[string]any{
		"id":               safe,
		"display_name":     safe,
		"username":         safe,
		"acct":             safe,
		"url":              accountURL,
		"uri":              accountURL,
		"created_at":       isoTime(epochUTC()),
		"locked":           false,
		"bot":              false,
		"discoverable":     true,
		"indexable":        false,
		"group":            false,
		"avatar":           nil,
		"avatar_static":    nil,
		"header":           nil,
		"header_static":    nil,
		"followers_count":  nil,
		"following_count":  nil,
		"statuses_count":   nil,
		"hide_collections": false,
		"noindex":          false,
		"emojis":           []any{},
		"roles":            []any{},
		"fields":           []any{},
	})
}

func (a *App) buildRateLimitActivityStatus(id string) []byte {
	now := isoTime(nowUTC())
	if id == "" {
		id = "rate-limit"
	}
	appURL := a.cfg.BaseURL
	if appURL == "" {
		appURL = instagramOrigin
	}
	return jsonBytes(map[string]any{
		"id":                     id,
		"url":                    instagramOrigin,
		"uri":                    instagramOrigin,
		"created_at":             now,
		"edited_at":              nil,
		"reblog":                 nil,
		"in_reply_to_id":         nil,
		"in_reply_to_account_id": nil,
		"content":                "<b>" + htmlEscape(rateLimitTitle) + "</b><p>" + htmlEscape(rateLimitDescription) + "</p>",
		"spoiler_text":           "",
		"language":               "en",
		"visibility":             "public",
		"application":            map[string]any{"name": a.cfg.BrandName, "website": nullableString(a.cfg.BaseURL)},
		"media_attachments":      []any{},
		"account": map[string]any{
			"id": a.cfg.BrandName, "display_name": a.cfg.BrandName, "username": a.cfg.BrandName,
			"acct": a.cfg.BrandName, "url": appURL, "uri": appURL, "created_at": now,
			"locked": false, "bot": true, "discoverable": false, "indexable": false, "group": false,
			"avatar": nil, "avatar_static": nil, "header": nil, "header_static": nil,
			"followers_count": nil, "following_count": nil, "statuses_count": nil,
			"hide_collections": false, "noindex": true, "emojis": []any{}, "roles": []any{}, "fields": []any{},
		},
		"mentions": []any{}, "tags": []any{}, "emojis": []any{}, "card": nil, "poll": nil,
	})
}

var activityMediaBase = new(big.Int).SetUint64(114163769487684704)

func buildActivityMediaAttachment(baseURL string, post Post, att Attachment, index int) map[string]any {
	sizeMultiplier := 1.0
	if att.Kind == "video" {
		if att.Width > 1920 || att.Height > 1920 {
			sizeMultiplier = 0.5
		}
		if att.Width < 400 && att.Height < 400 {
			sizeMultiplier = 2
		}
	}
	width := int(float64(att.Width)*sizeMultiplier + 0.5)
	height := int(float64(att.Height)*sizeMultiplier + 0.5)
	aspect := 1.0
	if height > 0 {
		aspect = float64(int(float64(width)/float64(height)*10000+0.5)) / 10000
	}
	size := strconv.Itoa(width) + "x" + strconv.Itoa(height)
	id := new(big.Int).Add(activityMediaBase, big.NewInt(int64(index))).String()

	mediaType := "image"
	var u, preview any
	switch {
	case att.Kind == "video" && !att.OversizedInline:
		mediaType = "video"
		u = offloadURL(baseURL, post.Shortcode, index, false)
		preview = offloadURL(baseURL, post.Shortcode, index, true)
	case att.Kind == "video":
		u = offloadURL(baseURL, post.Shortcode, index, true)
	default:
		u = att.URL
	}
	return map[string]any{
		"id":                 id,
		"type":               mediaType,
		"url":                u,
		"preview_url":        preview,
		"remote_url":         nil,
		"preview_remote_url": nil,
		"text_url":           nil,
		"description":        nil,
		"meta": map[string]any{
			"original": map[string]any{"width": width, "height": height, "aspect": aspect, "size": size},
			"width":    width, "height": height, "aspect": aspect, "size": size,
		},
	}
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}
