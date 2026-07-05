package main

import (
	"html"
	"net/http"
	"net/url"
	"strings"
)

const (
	asContext = "https://www.w3.org/ns/activitystreams"
	asPublic  = "https://www.w3.org/ns/activitystreams#Public"
)

func personObject(baseURL, username, name, profileURLStr, avatar string) map[string]any {
	if name == "" {
		name = username
	}
	actor := actorURL(baseURL, username)
	if profileURLStr == "" {
		profileURLStr = actor
	}
	p := map[string]any{
		"@context":          asContext,
		"id":                actor,
		"type":              "Person",
		"preferredUsername": username,
		"name":              name,
		"url":               profileURLStr,
		"inbox":             actor + "/inbox",
		"outbox":            actor + "/outbox",
		"followers":         actor + "/followers",
		"following":         actor + "/following",
	}
	if avatar != "" {
		p["icon"] = map[string]any{"type": "Image", "url": avatar}
	}
	return p
}

func noteObject(id string, attributedTo any, content, url, published string, attachment []any) []byte {
	if attachment == nil {
		attachment = []any{}
	}
	n := map[string]any{
		"@context":     asContext,
		"id":           id,
		"type":         "Note",
		"attributedTo": attributedTo,
		"content":      content,
		"url":          url,
		"to":           []any{asPublic},
		"attachment":   attachment,
	}
	if published != "" {
		n["published"] = published
	}
	return jsonBytes(n)
}

func mediaObject(mediaType, url string, width, height int) map[string]any {
	m := map[string]any{
		"type":      "Document",
		"mediaType": mediaType,
		"url":       url,
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

func (a *App) buildActivityStatus(baseURL string, post Post, postType string, mediaIndex int, specified, gallery bool) []byte {
	selectedIndex := mediaIndexFor(post, mediaIndex)

	postURL := baseURL + "/" + normalizePostType(postType) + "/" + url.PathEscape(post.Shortcode)
	id := statusURL(baseURL, post.Username, postType, post.Shortcode, selectedIndex, specified, gallery)
	selection := selectActivityAttachments(post, mediaIndex, specified)

	content := ""
	if !gallery {
		content = "<p><b>" + html.EscapeString(withIndicator(selection.indicator, post.StatsLine)) + "</b></p>"
		if caption := normalizeCaption(post.Caption); caption != "" {
			content += "<p>" + captionHTML(caption) + "</p>"
		}
	}

	attachment := make([]any, 0, len(selection.items))
	for _, it := range selection.items {
		attachment = append(attachment, activityAttachment(baseURL, post.Shortcode, it.att, it.index))
	}

	return noteObject(id, actorURL(baseURL, post.Username), content, postURL, isoTime(post.CreatedAt), attachment)
}

func activityAttachment(baseURL, shortcode string, att Attachment, index int) map[string]any {
	width, height := videoDisplaySize(att)
	if att.Kind == "video" {
		return mediaObject("video/mp4", offloadURL(baseURL, shortcode, index, false), width, height)
	}
	return mediaObject(imageMediaType(att.URL), offloadURL(baseURL, shortcode, index, false), width, height)
}

func imageMediaType(rawURL string) string {
	u := rawURL
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	switch {
	case strings.HasSuffix(u, ".png"):
		return "image/png"
	case strings.HasSuffix(u, ".webp"):
		return "image/webp"
	case strings.HasSuffix(u, ".heic"):
		return "image/heic"
	default:
		return "image/jpeg"
	}
}

func profileStatusURL(baseURL, username string) string {
	return actorURL(baseURL, username) + "/statuses/" + profileSnowcode(username)
}

func profileDigestContent(p Profile) string {
	content := "<p><b>" + html.EscapeString(profileStatsLine(p)) + "</b></p>"
	if bio := profileBioHTML(p); bio != "" {
		content += bio
	}
	if p.IsPrivate {
		content += "<p>" + html.EscapeString(profilePrivateNotice) + "</p>"
	}
	return content
}

func (a *App) buildProfileActivityStatus(baseURL string, p Profile) []byte {
	attachment := make([]any, 0, len(p.RecentMedia))
	for _, m := range p.RecentMedia {
		attachment = append(attachment, mediaObject(imageMediaType(m.Thumbnail), m.Thumbnail, m.Width, m.Height))
	}
	return noteObject(profileStatusURL(baseURL, p.Username), actorURL(baseURL, p.Username),
		profileDigestContent(p), baseURL+"/"+url.PathEscape(p.Username), "", attachment)
}

func (a *App) buildFallbackAccount(baseURL, username string) []byte {
	safe := strings.TrimSpace(username)
	if safe == "" {
		safe = a.cfg.BrandName
	}
	return jsonBytes(personObject(baseURL, safe, safe, actorURL(baseURL, safe), ""))
}

func (a *App) handleUserCollection(req *http.Request, username, name string) resp {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		r := textResp(405, "Method Not Allowed")
		r.headers["Allow"] = "GET, HEAD"
		return r
	}

	switch name {
	case "inbox", "outbox", "followers", "following":
		body := jsonBytes(map[string]any{
			"@context":     asContext,
			"id":           actorURL(a.publicBaseURL(req), username) + "/" + name,
			"type":         "OrderedCollection",
			"totalItems":   0,
			"orderedItems": []any{},
		})
		return cacheable(activityJSONResp(200, body), edgeCacheSeconds)
	default:
		return resp{status: 404, headers: map[string]string{}}
	}
}
