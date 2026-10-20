package main

import (
	"cmp"
	"html"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	asContext   = "https://www.w3.org/ns/activitystreams"
	asPublic    = "https://www.w3.org/ns/activitystreams#Public"
	asMediaType = "application/activity+json"
)

// Each card URL also serves its ActivityStreams object (SWICG ActivityPub
// Discovery, "Content negotiation"), so the canonical card URL is the object id.
// www. serves the same cards as its parent host.
func apOrigin(baseURL string) string { return strings.Replace(baseURL, "://www.", "://", 1) }

func actorID(baseURL, username string) string {
	return apOrigin(baseURL) + "/" + url.PathEscape(strings.ToLower(username))
}

// postRefs names the post card a request renders: every index form becomes
// /{p|reel}/{code}/{n} with the index clamped to the post, and other query noise
// is dropped. The host stays, so g. gallery Notes keep their own ids.
func postRefs(baseURL string, post Post, postType string, mediaIndex int, specified bool) (id, actor string) {
	id = apOrigin(baseURL) + "/" + normalizePostType(postType) + "/" + url.PathEscape(post.Shortcode)
	if specified {
		id += "/" + strconv.Itoa(mediaIndexFor(post, mediaIndex)+1)
	}
	return id, actorID(baseURL, post.Username)
}

func storyRefs(baseURL string, story Story) (id, actor string) {
	return apOrigin(baseURL) + "/stories/" + url.PathEscape(strings.ToLower(story.Username)) + "/" + url.PathEscape(story.ID),
		actorID(baseURL, story.Username)
}

// wantsActivity reports whether Accept ranks an AS2 type (activity+json, or
// ld+json with or without the AS profile) at least as high as HTML. Wildcards
// and a missing Accept get the card, so browsers and preview bots never see JSON.
// ponytail: splits on every comma, so a quoted parameter containing one is lost;
// the AS profile URL has none.
func wantsActivity(r *http.Request) bool {
	as2, htmlQ, htmlRank := 0.0, -1.0, 0
	for _, field := range r.Header.Values("Accept") {
		for _, part := range strings.Split(field, ",") {
			mediaType, params, err := mime.ParseMediaType(part)
			q := 1.0
			if s, ok := params["q"]; ok && err == nil {
				q, err = strconv.ParseFloat(s, 64)
			}
			if err != nil {
				continue
			}
			// The most specific range that covers HTML sets its quality.
			rank := 0
			switch mediaType {
			case asMediaType, "application/ld+json":
				as2 = max(as2, q)
			case "text/html":
				rank = 3
			case "text/*":
				rank = 2
			case "*/*":
				rank = 1
			}
			if rank > htmlRank || rank > 0 && rank == htmlRank && q > htmlQ {
				htmlQ, htmlRank = q, rank
			}
		}
	}
	return as2 > 0 && as2 >= htmlQ
}

func linkValue(href, rel, mediaType string) string {
	return "<" + href + `>; rel="` + rel + `"; type="` + mediaType + `"`
}

// apLinks is the Link header advertising a card's alternate (idType) and author
// (SWICG "Publishing HTML"); an actor's own card passes no author.
func apLinks(id, idType, actor string) string {
	header := linkValue(id, "alternate", idType)
	if actor != "" {
		header += ", " + linkValue(actor, "author", asMediaType)
	}
	return header
}

// apLinkTags are the <head> twins of apLinks: one alternate and one author link.
func apLinkTags(id, actor string) []string {
	tags := []string{`<link rel="alternate" type="` + asMediaType + `" href="` + html.EscapeString(id) + `">`}
	if actor != "" {
		tags = append(tags, `<link rel="author" type="`+asMediaType+`" href="`+html.EscapeString(actor)+`">`)
	}
	return tags
}

// The HTML card shares the object's URL; Vary because the card route negotiates.
func activityResp(body []byte, id, actor string) resp {
	return resp{status: http.StatusOK, headers: map[string]string{"Content-Type": asMediaType, "Vary": "Accept", "Link": apLinks(id, "text/html", actor)}, body: body}
}

// Federated clients get a fixed verdict, never upstream statuses or helper messages.
func activityErrorResp(err *AppError) resp {
	status, detail := http.StatusNotFound, "not found"
	switch {
	case isTransient(err.Code):
		status, detail = http.StatusServiceUnavailable, "temporarily unavailable"
	case err.Code == errorCodeRestricted || err.Code == errorCodeGeoBlocked:
		status, detail = http.StatusUnavailableForLegalReasons, "restricted by Instagram"
	}
	r := problemResp(status, detail)
	r.headers["Vary"] = "Accept"
	if status == http.StatusServiceUnavailable {
		r.headers["Retry-After"] = strconv.Itoa(transientErrorCacheSeconds)
	}
	r.originStatus, r.errorType = err.Status, err.Code
	return r
}

func htmlLink(href string) map[string]any {
	return map[string]any{"type": "Link", "href": href, "mediaType": "text/html"}
}

func emptyOrderedCollection(id, actor string) []byte {
	return jsonBytes(map[string]any{
		"@context":     asContext,
		"id":           id,
		"type":         "OrderedCollection",
		"attributedTo": actor,
		"totalItems":   0,
		"orderedItems": []any{},
	})
}

func noteObject(id, actor, content, published string, attachment []any) []byte {
	n := map[string]any{
		"@context":     asContext,
		"id":           id,
		"type":         "Note",
		"attributedTo": actor,
		"content":      content,
		"url":          htmlLink(id),
		"to":           []any{asPublic},
		"attachment":   attachment,
	}
	if published != "" {
		n["published"] = published
	}
	return jsonBytes(n)
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

func (a *App) postNote(baseURL string, post Post, postType string, mediaIndex int, specified, gallery bool) []byte {
	id, actor := postRefs(baseURL, post, postType, mediaIndex, specified)
	selection := selectActivityAttachments(post, mediaIndex, specified)
	attachment := make([]any, 0, len(selection.items))
	for _, it := range selection.items {
		attachment = append(attachment, activityAttachment(a.offloadURL(baseURL, post.Shortcode, it.index, false), it.att))
	}
	return noteObject(id, actor, postStatusContent(post, selection.indicator, gallery), isoTime(post.CreatedAt), attachment)
}

func activityAttachment(mediaURL string, att Attachment) map[string]any {
	m := map[string]any{"type": "Document", "mediaType": "image/jpeg", "url": mediaURL}
	if att.Kind == "video" {
		m["mediaType"] = "video/mp4"
	}
	width, height := videoDisplaySize(att)
	if width > 0 {
		m["width"] = width
	}
	if height > 0 {
		m["height"] = height
	}
	return m
}

func (a *App) storyNote(baseURL string, story Story, gallery bool) []byte {
	id, actor := storyRefs(baseURL, story)
	media := activityAttachment(a.storyOffloadURL(baseURL, story.Username, story.ID, false), story.Media)
	return noteObject(id, actor, statusContent("", story.Caption, gallery), isoTime(story.CreatedAt), []any{media})
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

// The collections exist because ActivityPub requires inbox and outbox; this
// server never delivers or accepts activities, so they stay empty. Without a
// publicKey GoToSocial rejects the actor and so every Note attributed to it,
// whatever its fetch mode; Mastodon and Misskey resolve both.
func (a *App) profileActor(baseURL string, p Profile, gallery bool) []byte {
	id := actorID(baseURL, p.Username)
	person := map[string]any{
		"@context":          asContext,
		"id":                id,
		"type":              "Person",
		"preferredUsername": strings.ToLower(p.Username),
		"name":              cmp.Or(p.FullName, p.Username),
		"icon":              map[string]any{"type": "Image", "mediaType": "image/jpeg", "url": a.profileAvatarURL(baseURL, p)},
		"url":               htmlLink(id),
		"inbox":             id + "/inbox",
		"outbox":            id + "/outbox",
		"followers":         id + "/followers",
		"following":         id + "/following",
	}
	if summary := profileDigestContent(p, gallery); summary != "" {
		person["summary"] = summary
	}
	return jsonBytes(person)
}
