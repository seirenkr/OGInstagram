package main

import (
	"time"

	"github.com/tidwall/gjson"
)

func parseInstagramPost(body string) (Post, *AppError) {
	root := gjson.Parse(body)
	data := root.Get("data")
	if m := data.Get("xdt_shortcode_media"); present(m) {
		return parseXDT(m)
	}
	if m := data.Get("shortcode_media"); present(m) {
		return parseXDT(m)
	}
	if m := data.Get("xdt_api__v1__media__shortcode__web_info.items.0"); present(m) {
		return parseV1(m)
	}
	if data.Exists() {

		return Post{}, igErr(404, reasonMediaNotFound, "Sorry, this page isn't available. The link you followed may be broken, or the page may have been removed.")
	}

	return Post{}, igErr(502, reasonGraphql, "Instagram response did not include media")
}

func parseXDT(media gjson.Result) (Post, *AppError) {
	owner := media.Get("owner")
	if !owner.Exists() {
		return Post{}, igErr(502, reasonClientError, "missing owner")
	}
	username := owner.Get("username").String()
	fullName := owner.Get("full_name").String()
	if fullName == "" {
		fullName = username
	}
	typename := media.Get("__typename").String()

	var attachments []Attachment
	statsPrefix := ""
	switch {
	case typename == "XDTGraphSidecar" || typename == "GraphSidecar":
		for _, edge := range media.Get("edge_sidecar_to_children.edges").Array() {
			if att, ok := parseXDTAttachment(edge.Get("node")); ok {
				attachments = append(attachments, att)
			}
		}
	case isVideoMedia(media):
		if att, ok := parseXDTAttachment(media); ok {
			attachments = append(attachments, att)
		}
		play := uintOf(media, "video_play_count")
		if play == 0 {
			play = uintOf(media, "video_view_count")
		}
		if play > 0 {
			statsPrefix = "▶️ " + fmtCount(play) + "  "
		}
	default:
		if att, ok := parseXDTAttachment(media); ok {
			attachments = append(attachments, att)
		}
	}
	if len(attachments) == 0 {
		return Post{}, igErr(502, reasonClientError, "media had no usable attachments")
	}

	likes := uintOf(media, "edge_media_preview_like.count")
	comments := uintOf(media, "edge_media_to_parent_comment.count")
	return Post{
		Shortcode:   media.Get("shortcode").String(),
		Username:    username,
		FullName:    fullName,
		ProfilePic:  normalizeCDNHost(owner.Get("profile_pic_url").String()),
		Caption:     media.Get("edge_media_to_caption.edges.0.node.text").String(),
		StatsLine:   statsPrefix + "❤️ " + fmtCount(likes) + "  \U0001f4ac " + fmtCount(comments),
		Attachments: attachments,
		CreatedAt:   unixTime(media.Get("taken_at_timestamp").Int()),
	}, nil
}

func parseXDTAttachment(node gjson.Result) (Attachment, bool) {
	if !node.Exists() {
		return Attachment{}, false
	}
	thumbnail := bestDisplayURL(node)
	if thumbnail == "" {
		return Attachment{}, false
	}
	att := Attachment{
		Kind:      "image",
		URL:       thumbnail,
		Thumbnail: thumbnail,
		Width:     mediaWidth(node),
		Height:    mediaHeight(node),
	}
	if isVideoMedia(node) {
		att.Kind = "video"
		if v := bestVideoURL(node); v != "" {
			att.URL = v
		}
	} else if d := node.Get("display_url").String(); d != "" {
		att.URL = d
	}
	att.URL = normalizeCDNHost(att.URL)
	att.Thumbnail = normalizeCDNHost(att.Thumbnail)
	return att, true
}

func parseV1(item gjson.Result) (Post, *AppError) {
	user := item.Get("user")
	if !user.Exists() {
		return Post{}, igErr(502, reasonClientError, "missing user")
	}
	username := user.Get("username").String()
	fullName := user.Get("full_name").String()
	if fullName == "" {
		fullName = username
	}
	shortcode := item.Get("code").String()
	if shortcode == "" {
		shortcode = item.Get("shortcode").String()
	}

	var attachments []Attachment
	carousel := item.Get("carousel_media").Array()
	if len(carousel) > 0 {
		for _, child := range carousel {
			if att, ok := parseV1Attachment(child); ok {
				attachments = append(attachments, att)
			}
		}
	} else if att, ok := parseV1Attachment(item); ok {
		attachments = append(attachments, att)
	}
	if len(attachments) == 0 {
		return Post{}, igErr(502, reasonClientError, "v1 media had no usable attachments")
	}

	created := item.Get("taken_at").Int()
	if created == 0 {
		created = item.Get("taken_at_timestamp").Int()
	}
	caption := item.Get("caption.text").String()
	if caption == "" {
		caption = item.Get("caption_text").String()
	}
	return Post{
		Shortcode:   shortcode,
		Username:    username,
		FullName:    fullName,
		ProfilePic:  normalizeCDNHost(user.Get("profile_pic_url").String()),
		Caption:     caption,
		StatsLine:   v1StatsPrefix(item) + "❤️ " + fmtCount(uintOf(item, "like_count")) + "  \U0001f4ac " + fmtCount(uintOf(item, "comment_count")),
		Attachments: attachments,
		CreatedAt:   unixTime(created),
	}, nil
}

func parseV1Attachment(item gjson.Result) (Attachment, bool) {
	thumbnail := bestV1ImageURL(item)
	if thumbnail == "" {
		return Attachment{}, false
	}
	w, h := mediaWidth(item), mediaHeight(item)
	thumbnail = normalizeCDNHost(thumbnail)
	if uintOf(item, "media_type") == 2 {
		u := bestVideoURL(item)
		if u == "" {
			u = thumbnail
		}
		return Attachment{Kind: "video", URL: normalizeCDNHost(u), Thumbnail: thumbnail, Width: w, Height: h}, true
	}
	return Attachment{Kind: "image", URL: thumbnail, Thumbnail: thumbnail, Width: w, Height: h}, true
}

func bestV1ImageURL(item gjson.Result) string {
	if u := bestCandidateURL(item.Get("image_versions2.candidates")); u != "" {
		return u
	}
	return item.Get("thumbnail_url").String()
}

func bestDisplayURL(node gjson.Result) string {
	if u := bestCandidateURL(node.Get("display_resources")); u != "" {
		return u
	}
	if u := bestCandidateURL(node.Get("thumbnail_resources")); u != "" {
		return u
	}
	for _, k := range []string{"display_url", "thumbnail_url", "thumbnail_src"} {
		if u := node.Get(k).String(); u != "" {
			return u
		}
	}
	return ""
}

func bestVideoURL(node gjson.Result) string {
	if u := node.Get("video_url").String(); u != "" {
		return u
	}
	if u := bestCandidateURL(node.Get("video_versions")); u != "" {
		return u
	}
	return bestCandidateURL(node.Get("video_resources"))
}

func bestCandidateURL(value gjson.Result) string {
	bestURL := ""
	bestArea := -1
	for _, c := range value.Array() {
		u := c.Get("url").String()
		if u == "" {
			u = c.Get("src").String()
		}
		if u == "" {
			continue
		}
		area := candidateWidth(c) * candidateHeight(c)
		if bestURL == "" || area > bestArea {
			bestURL = u
			bestArea = area
		}
	}
	return bestURL
}

func isVideoMedia(value gjson.Result) bool {
	tn := value.Get("__typename").String()
	return value.Get("is_video").Bool() || uintOf(value, "media_type") == 2 || tn == "XDTGraphVideo" || tn == "GraphVideo"
}

func mediaWidth(value gjson.Result) int {
	if w := uintOf(value, "dimensions.width"); w > 0 {
		return w
	}
	if w := uintOf(value, "original_width"); w > 0 {
		return w
	}
	if w := uintOf(value, "width"); w > 0 {
		return w
	}
	return candidateWidth(bestImageCandidate(value))
}

func mediaHeight(value gjson.Result) int {
	if h := uintOf(value, "dimensions.height"); h > 0 {
		return h
	}
	if h := uintOf(value, "original_height"); h > 0 {
		return h
	}
	if h := uintOf(value, "height"); h > 0 {
		return h
	}
	return candidateHeight(bestImageCandidate(value))
}

func bestImageCandidate(value gjson.Result) gjson.Result {
	if c := bestCandidate(value.Get("image_versions2.candidates")); c.Exists() {
		return c
	}
	if c := bestCandidate(value.Get("display_resources")); c.Exists() {
		return c
	}
	return bestCandidate(value.Get("thumbnail_resources"))
}

func bestCandidate(value gjson.Result) gjson.Result {
	var best gjson.Result
	bestArea := -1
	for _, c := range value.Array() {
		area := candidateWidth(c) * candidateHeight(c)
		if !best.Exists() || area > bestArea {
			best = c
			bestArea = area
		}
	}
	return best
}

func candidateWidth(value gjson.Result) int {
	if w := uintOf(value, "width"); w > 0 {
		return w
	}
	return uintOf(value, "config_width")
}

func candidateHeight(value gjson.Result) int {
	if h := uintOf(value, "height"); h > 0 {
		return h
	}
	return uintOf(value, "config_height")
}

func v1StatsPrefix(item gjson.Result) string {
	play := uintOf(item, "play_count")
	for _, k := range []string{"video_play_count", "view_count", "video_view_count"} {
		if play > 0 {
			break
		}
		play = uintOf(item, k)
	}
	if play > 0 {
		return "▶️ " + fmtCount(play) + "  "
	}
	return ""
}

func present(r gjson.Result) bool { return r.Exists() && r.Type != gjson.Null }

func uintOf(value gjson.Result, path string) int {
	n := value.Get(path).Int()
	if n < 0 {
		return 0
	}
	return int(n)
}

func unixTime(seconds int64) time.Time {
	if seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	return time.Now().UTC()
}
