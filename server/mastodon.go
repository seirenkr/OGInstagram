package main

import (
	"cmp"
	"html"
	"net/url"
	"strconv"
	"time"
)

func mastodonTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func mastodonDate(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02")
}

func mastodonPolicy(currentUser string) map[string]any {
	return map[string]any{
		"automatic":    []any{},
		"manual":       []any{},
		"current_user": currentUser,
	}
}

func mastodonStatusJSON(id, uri, statusURL, content string, createdAt time.Time, account map[string]any, media []any) []byte {
	return jsonBytes(map[string]any{
		"id":                     id,
		"uri":                    uri,
		"url":                    statusURL,
		"created_at":             mastodonTime(createdAt),
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
		"quotes_count":           0,
		"text":                   nil,
		"quote":                  nil,
		"quote_approval":         mastodonPolicy("denied"),
		"tagged_collections":     []any{},
		"account":                account,
		"media_attachments":      media,
		"mentions":               []any{},
		"tags":                   []any{},
		"emojis":                 []any{},
	})
}

func mastodonMediaObject(id, kind, mediaURL, previewURL string, width, height int) map[string]any {
	aspect := 0.0
	if height > 0 {
		aspect = float64(width) / float64(height)
	}
	return map[string]any{
		"id":          id,
		"type":        kind,
		"url":         mediaURL,
		"preview_url": previewURL,
		"remote_url":  nil,
		"meta": map[string]any{
			"original": map[string]any{"width": width, "height": height, "aspect": aspect},
			"small":    map[string]any{"width": width, "height": height, "aspect": aspect},
		},
		"description": nil,
		"blurhash":    nil,
	}
}

func (a *App) mastodonMedia(baseURL, shortcode string, att Attachment, index int) map[string]any {
	id := att.ID
	if id == "" {
		if pk := shortcodePK(shortcode); pk != nil {
			id = pk.String()
		}
	}
	return mastodonMediaObject(id, att.Kind,
		a.offloadURL(baseURL, shortcode, index, false),
		a.offloadURL(baseURL, shortcode, index, att.Kind == "video"),
		att.Width, att.Height)
}

type mastodonAccountData struct {
	ID             string
	Username       string
	FullName       string
	Avatar         string
	Note           string
	LastStatusAt   time.Time
	FollowersCount int
	FollowingCount int
	StatusesCount  int
}

func mastodonAccount(data mastodonAccountData) map[string]any {
	var note any
	if data.Note != "" {
		note = data.Note
	}
	return map[string]any{
		"id":                 cmp.Or(data.ID, data.Username),
		"username":           data.Username,
		"acct":               data.Username,
		"url":                profileURL(data.Username),
		"uri":                profileURL(data.Username),
		"display_name":       cmp.Or(data.FullName, data.Username),
		"note":               note,
		"avatar":             data.Avatar,
		"avatar_static":      data.Avatar,
		"avatar_description": nil,
		"header":             nil,
		"header_static":      nil,
		"header_description": nil,
		"locked":             false,
		"bot":                false,
		"group":              false,
		"discoverable":       nil,
		"indexable":          false,
		"created_at":         nil,
		"last_status_at":     mastodonDate(data.LastStatusAt),
		"followers_count":    data.FollowersCount,
		"following_count":    data.FollowingCount,
		"statuses_count":     data.StatusesCount,
		"hide_collections":   nil,
		"show_media":         false,
		"show_media_replies": false,
		"show_featured":      false,
		"roles":              []any{},
		"feature_approval":   mastodonPolicy("missing"),
		"emojis":             []any{},
		"fields":             []any{},
	}
}

func profileMastodonMedia(m ProfileMedia, index int, mediaURL string) map[string]any {
	id := m.ID
	if id == "" {
		id = strconv.Itoa(index)
	}
	return mastodonMediaObject(id, "image", mediaURL, mediaURL, m.Width, m.Height)
}

func (a *App) buildStoryMastodonStatus(baseURL string, story Story, gallery bool) []byte {
	account := mastodonAccount(mastodonAccountData{
		Username:     story.Username,
		FullName:     story.FullName,
		Avatar:       a.storyAvatarURL(baseURL, story),
		LastStatusAt: story.CreatedAt,
	})
	return mastodonStatusJSON(
		storyStatusSnowcode(story.Username, story.ID, gallery),
		storyStatusURL(baseURL, story.Username, story.ID, gallery),
		storyOriginURL(story.Username, story.ID),
		statusContent("", story.Caption, gallery), story.CreatedAt, account,
		[]any{mastodonMediaObject(cmp.Or(story.Media.ID, story.ID), story.Media.Kind,
			a.storyOffloadURL(baseURL, story.Username, story.ID, false),
			a.storyOffloadURL(baseURL, story.Username, story.ID, story.Media.Kind == "video"),
			story.Media.Width, story.Media.Height)})
}

func (a *App) buildMastodonProfileStatus(baseURL string, p Profile) []byte {
	media := make([]any, 0, len(p.RecentMedia))
	for i, m := range p.RecentMedia {
		media = append(media, profileMastodonMedia(m, i, a.profileMediaOffloadURL(baseURL, p.Username, i)))
	}

	var created time.Time
	if len(p.RecentMedia) > 0 && !p.RecentMedia[0].TakenAt.IsZero() {
		created = p.RecentMedia[0].TakenAt
	}
	account := mastodonAccount(mastodonAccountData{
		ID:             p.UserID,
		Username:       p.Username,
		FullName:       p.FullName,
		Avatar:         a.profileAvatarURL(baseURL, p),
		Note:           captionParagraphHTML(p.Biography),
		LastStatusAt:   created,
		FollowersCount: p.FollowerCount,
		FollowingCount: p.FollowingCount,
		StatusesCount:  p.MediaCount,
	})
	return mastodonStatusJSON(
		profileSnowcode(p.Username),
		profileStatusURL(baseURL, p.Username),
		baseURL+"/"+url.PathEscape(p.Username),
		profileDigestContent(p), created, account, media)
}

func (a *App) buildMastodonStatus(baseURL string, post Post, postType string, mediaIndex int, specified, gallery bool) []byte {
	selection := selectActivityAttachments(post, mediaIndex, specified)
	media := make([]any, 0, len(selection.items))
	for _, it := range selection.items {
		media = append(media, a.mastodonMedia(baseURL, post.Shortcode, it.att, it.index))
	}

	account := mastodonAccount(mastodonAccountData{
		ID:           post.OwnerID,
		Username:     post.Username,
		FullName:     post.FullName,
		Avatar:       a.postAvatarURL(baseURL, post),
		LastStatusAt: post.CreatedAt,
	})
	return mastodonStatusJSON(
		statusSnowcode(postType, post.Shortcode, mediaIndex, specified, gallery),
		statusURL(baseURL, post.Username, postType, post.Shortcode, mediaIndex, specified, gallery),
		baseURL+"/"+normalizePostType(postType)+"/"+url.PathEscape(post.Shortcode),
		statusContent("<p><b>"+html.EscapeString(withIndicator(selection.indicator, post.StatsLine))+"</b></p>", post.Caption, gallery),
		post.CreatedAt, account, media)
}
