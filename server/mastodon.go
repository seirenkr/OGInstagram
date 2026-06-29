package main

import (
	"strconv"
	"time"
)

func mastodonTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func mastodonMedia(baseURL, shortcode string, att Attachment, index int) map[string]any {
	w, h := att.Width, att.Height
	aspect := 0.0
	if h > 0 {
		aspect = float64(w) / float64(h)
	}
	meta := map[string]any{
		"original": map[string]any{"width": w, "height": h, "aspect": aspect},
		"small":    map[string]any{"width": w, "height": h, "aspect": aspect},
	}

	kind := att.Kind

	previewURL := offloadURL(baseURL, shortcode, index, kind == "video")

	id := att.ID
	if id == "" {
		id = shortcodeToPK(shortcode)
	}
	return map[string]any{
		"id":          id,
		"type":        kind,
		"url":         offloadURL(baseURL, shortcode, index, false),
		"preview_url": previewURL,
		"remote_url":  nil,
		"meta":        meta,
		"description": nil,
		"blurhash":    nil,
	}
}

func (a *App) mastodonAccount(accountID, username, fullName, avatar string, created time.Time) map[string]any {
	display := fullName
	if display == "" {
		display = username
	}
	if accountID == "" {
		accountID = username
	}
	return map[string]any{
		"id":           accountID,
		"username":     username,
		"acct":         username,
		"display_name": display,
		"locked":       false,
		"bot":          false,
		"discoverable": true,
		"group":        false,
		// These are spec non-nullable; null makes Discord drop the status (breaks video embeds).
		"created_at":      mastodonTime(created),
		"last_status_at":  created.UTC().Format("2006-01-02"),
		"note":            "",
		"url":             profileURL(username),
		"avatar":          avatar,
		"avatar_static":   avatar,
		"header":          "",
		"header_static":   "",
		"followers_count": 0,
		"following_count": 0,
		"statuses_count":  0,
		"emojis":          []any{},
		"fields":          []any{},
	}
}

func profileMastodonMedia(m ProfileMedia, index int) map[string]any {
	aspect := 0.0
	if m.Height > 0 {
		aspect = float64(m.Width) / float64(m.Height)
	}
	id := m.ID
	if id == "" {
		id = strconv.Itoa(index)
	}
	return map[string]any{
		"id":          id,
		"type":        "image",
		"url":         m.Thumbnail,
		"preview_url": m.Thumbnail,
		"remote_url":  nil,
		"meta": map[string]any{
			"original": map[string]any{"width": m.Width, "height": m.Height, "aspect": aspect},
			"small":    map[string]any{"width": m.Width, "height": m.Height, "aspect": aspect},
		},
		"description": nil,
		"blurhash":    nil,
	}
}

func (a *App) buildMastodonProfileStatus(baseURL string, p Profile) []byte {
	media := make([]any, 0, len(p.RecentMedia))
	for i, m := range p.RecentMedia {
		media = append(media, profileMastodonMedia(m, i))
	}

	created := nowUTC()
	if len(p.RecentMedia) > 0 && !p.RecentMedia[0].TakenAt.IsZero() {
		created = p.RecentMedia[0].TakenAt
	}
	status := map[string]any{
		"id":                     profileSnowcode(p.Username),
		"uri":                    profileStatusURL(baseURL, p.Username),
		"url":                    baseURL + "/" + pathEscape(p.Username),
		"created_at":             mastodonTime(created),
		"edited_at":              nil,
		"in_reply_to_id":         nil,
		"in_reply_to_account_id": nil,
		"reblog":                 nil,
		"poll":                   nil,
		"card":                   nil,
		"language":               nil,
		"content":                profileDigestContent(p),
		"visibility":             "public",
		"sensitive":              false,
		"spoiler_text":           "",
		"replies_count":          0,
		"reblogs_count":          0,
		"favourites_count":       0,
		"account":                a.mastodonAccount(p.UserID, p.Username, profileDisplayName(p), p.ProfilePic, created),
		"media_attachments":      media,
		"mentions":               []any{},
		"tags":                   []any{},
		"emojis":                 []any{},
	}
	return jsonBytes(status)
}

func (a *App) buildMastodonStatus(baseURL string, post Post, postType string, mediaIndex int, specified, gallery bool) []byte {
	selection := selectActivityAttachments(post, mediaIndex, specified)
	media := make([]any, 0, len(selection.items))
	for _, it := range selection.items {
		media = append(media, mastodonMedia(baseURL, post.Shortcode, it.att, it.index))
	}

	content := ""
	if !gallery {
		content = "<p><b>" + htmlEscape(withIndicator(selection.indicator, post.StatsLine)) + "</b></p>"
		if caption := normalizeCaption(post.Caption); caption != "" {
			content += "<p>" + captionHTML(caption) + "</p>"
		}
	}

	status := map[string]any{
		"id":                     statusSnowcode(postType, post.Shortcode, mediaIndex, specified, gallery),
		"uri":                    statusURL(baseURL, post.Username, postType, post.Shortcode, mediaIndex, specified, false),
		"url":                    baseURL + "/" + normalizePostType(postType) + "/" + pathEscape(post.Shortcode),
		"created_at":             mastodonTime(post.CreatedAt),
		"edited_at":              nil,
		"in_reply_to_id":         nil,
		"in_reply_to_account_id": nil,
		"reblog":                 nil,
		"poll":                   nil,
		"card":                   nil,
		"language":               nil,
		"content":                content,
		"visibility":             "public",
		"sensitive":              false,
		"spoiler_text":           "",
		"replies_count":          0,
		"reblogs_count":          0,
		"favourites_count":       0,
		"account":                a.mastodonAccount(post.OwnerID, post.Username, post.FullName, post.ProfilePic, post.CreatedAt),
		"media_attachments":      media,
		"mentions":               []any{},
		"tags":                   []any{},
		"emojis":                 []any{},
	}
	return jsonBytes(status)
}
