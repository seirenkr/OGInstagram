package main

import (
	"html"
	"net/url"
)

const (
	asContext = "https://www.w3.org/ns/activitystreams"
	asPublic  = "https://www.w3.org/ns/activitystreams#Public"
)

func emptyOrderedCollection(id string) []byte {
	return jsonBytes(map[string]any{
		"@context":     asContext,
		"id":           id,
		"type":         "OrderedCollection",
		"totalItems":   0,
		"orderedItems": []any{},
	})
}

func noteObject(id, attributedTo, content, href, published string, attachment []any) []byte {
	n := map[string]any{
		"@context":     asContext,
		"id":           id,
		"type":         "Note",
		"attributedTo": attributedTo,
		"content":      content,
		"url":          href,
		"to":           []any{asPublic},
		"attachment":   attachment,
	}
	if published != "" {
		n["published"] = published
	}
	return jsonBytes(n)
}

func mediaObject(mediaType, href string, width, height int) map[string]any {
	m := map[string]any{
		"type":      "Document",
		"mediaType": mediaType,
		"url":       href,
	}
	if width > 0 {
		m["width"] = width
	}
	if height > 0 {
		m["height"] = height
	}
	return m
}

func actorURL(baseURL, username string) string {
	return baseURL + "/users/" + url.PathEscape(username)
}

func statusURL(baseURL, username, postType, shortcode string, mediaIndex int, specified, gallery bool) string {
	return actorURL(baseURL, username) + "/statuses/" +
		statusSnowcode(postType, shortcode, mediaIndex, specified, gallery)
}

func statusContent(prefix, caption string, gallery bool) string {
	if gallery {
		return ""
	}
	return prefix + captionParagraphHTML(caption)
}

func postStatusContent(post Post, indicator string, gallery bool) string {
	var stats string
	if line := withIndicator(indicator, post.StatsLine); line != "" {
		stats = "<p><b>" + html.EscapeString(line) + "</b></p>"
	}
	return statusContent(stats, post.Caption, gallery)
}

func (a *App) buildActivityStatus(baseURL string, post Post, postType string, mediaIndex int, specified, gallery bool) []byte {
	selectedIndex := mediaIndexFor(post, mediaIndex)

	postURL := baseURL + "/" + normalizePostType(postType) + "/" + url.PathEscape(post.Shortcode)
	id := statusURL(baseURL, post.Username, postType, post.Shortcode, selectedIndex, specified, gallery)
	selection := selectActivityAttachments(post, mediaIndex, specified)

	content := postStatusContent(post, selection.indicator, gallery)

	attachment := make([]any, 0, len(selection.items))
	for _, it := range selection.items {
		attachment = append(attachment, activityAttachment(a.offloadURL(baseURL, post.Shortcode, it.index, false), it.att))
	}

	return noteObject(id, actorURL(baseURL, post.Username), content, postURL, isoTime(post.CreatedAt), attachment)
}

func activityAttachment(mediaURL string, att Attachment) map[string]any {
	width, height := videoDisplaySize(att)
	if att.Kind == "video" {
		return mediaObject("video/mp4", mediaURL, width, height)
	}
	return mediaObject("image/jpeg", mediaURL, width, height)
}

func profileStatusURL(baseURL, username string, gallery bool) string {
	return actorURL(baseURL, username) + "/statuses/" + profileStatusSnowcode(username, gallery)
}

func storyStatusURL(baseURL, username, id string, gallery bool) string {
	return actorURL(baseURL, username) + "/statuses/" + storyStatusSnowcode(username, id, gallery)
}

func (a *App) buildStoryActivityStatus(baseURL string, story Story, gallery bool) []byte {
	media := activityAttachment(a.storyOffloadURL(baseURL, story.Username, story.ID, false), story.Media)

	return noteObject(storyStatusURL(baseURL, story.Username, story.ID, gallery), actorURL(baseURL, story.Username),
		statusContent("", story.Caption, gallery), storyOriginURL(story.Username, story.ID), isoTime(story.CreatedAt), []any{media})
}

func profileDigestContent(p Profile, gallery bool) string {
	if gallery {
		return ""
	}
	content := "<p><b>" + html.EscapeString(profileStatsLine(p)) + "</b></p>"
	content += captionParagraphHTML(p.Biography)
	if p.IsPrivate {
		content += "<p>" + html.EscapeString(profilePrivateNotice) + "</p>"
	}
	return content
}

func (a *App) buildProfileActivityStatus(baseURL string, p Profile, gallery bool) []byte {
	attachment := make([]any, 0, len(p.RecentMedia))
	for i, m := range p.RecentMedia {
		attachment = append(attachment, mediaObject("image/jpeg", a.profileMediaOffloadURL(baseURL, p.Username, i), m.Width, m.Height))
	}
	return noteObject(profileStatusURL(baseURL, p.Username, gallery), actorURL(baseURL, p.Username),
		profileDigestContent(p, gallery), baseURL+"/"+url.PathEscape(p.Username), "", attachment)
}

func (a *App) buildFallbackAccount(baseURL, username string) []byte {
	actor := actorURL(baseURL, username)
	return jsonBytes(map[string]any{
		"@context":          asContext,
		"id":                actor,
		"type":              "Person",
		"preferredUsername": username,
		"name":              username,
		"url":               actor,
		"inbox":             actor + "/inbox",
		"outbox":            actor + "/outbox",
	})
}
