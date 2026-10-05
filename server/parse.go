package main

import (
	"cmp"
	"time"

	"github.com/tidwall/gjson"
)

func parseInstagramPost(body string) (Post, *AppError) {
	root := gjson.Parse(body)
	data := root.Get("data")

	if it := data.Get("xdt_api__v1__media__shortcode__web_info.items.0"); present(it) {
		return parseV1(it)
	}
	if data.Exists() {
		return Post{}, igErr(404, errorCodeMediaNotFound, "Sorry, this page isn't available. The link you followed may be broken, or the page may have been removed.")
	}
	return Post{}, igErr(502, errorCodeGraphQL, "Instagram response did not include media")
}

func parseV1(item gjson.Result) (Post, *AppError) {
	user := item.Get("user")
	if !user.Exists() {
		return Post{}, igErr(502, errorCodeUpstream, "missing user")
	}
	username := user.Get("username").String()
	fullName := user.Get("full_name").String()

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
		return Post{}, igErr(502, errorCodeUpstream, "v1 media had no usable attachments")
	}

	return Post{
		Shortcode:   cmp.Or(item.Get("code").String(), item.Get("shortcode").String()),
		Username:    username,
		OwnerID:     cmp.Or(user.Get("pk").String(), user.Get("id").String()),
		FullName:    fullName,
		ProfilePic:  user.Get("profile_pic_url").String(),
		Caption:     cmp.Or(item.Get("caption.text").String(), item.Get("caption_text").String()),
		StatsLine:   statsLine(v1StatsPrefix(item), uintOf(item, "like_count"), uintOf(item, "comment_count")),
		Attachments: attachments,
		CreatedAt:   unixTime(cmp.Or(item.Get("taken_at").Int(), item.Get("taken_at_timestamp").Int())),
	}, nil
}

func parseV1Attachment(item gjson.Result) (Attachment, bool) {
	thumbnail := bestV1ImageURL(item)
	if thumbnail == "" {
		return Attachment{}, false
	}
	w, h := mediaWidth(item), mediaHeight(item)
	id := cmp.Or(item.Get("pk").String(), item.Get("id").String())
	if uintOf(item, "media_type") == 2 {
		return Attachment{ID: id, Kind: "video", URL: cmp.Or(bestVideoURL(item), thumbnail), Thumbnail: thumbnail, Width: w, Height: h}, true
	}
	return Attachment{ID: id, Kind: "image", URL: thumbnail, Thumbnail: thumbnail, Width: w, Height: h}, true
}

func bestV1ImageURL(item gjson.Result) string {
	return cmp.Or(bestCandidateURL(item.Get("image_versions2.candidates")),
		item.Get("thumbnail_url").String(), item.Get("display_url").String(),
		item.Get("thumbnail_src").String(), item.Get("display_src").String())
}

func bestVideoURL(node gjson.Result) string {
	return cmp.Or(node.Get("video_url").String(),
		bestCandidateURL(node.Get("video_versions")), bestCandidateURL(node.Get("video_resources")))
}

func bestCandidateURL(value gjson.Result) string {
	return candidateURL(bestCandidate(value))
}

func candidateURL(c gjson.Result) string {
	return cmp.Or(c.Get("url").String(), c.Get("src").String())
}

func mediaWidth(v gjson.Result) int {
	return cmp.Or(uintOf(v, "dimensions.width"), uintOf(v, "original_width"), uintOf(v, "width"),
		candidateWidth(bestImageCandidate(v)))
}

func mediaHeight(v gjson.Result) int {
	return cmp.Or(uintOf(v, "dimensions.height"), uintOf(v, "original_height"), uintOf(v, "height"),
		candidateHeight(bestImageCandidate(v)))
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
		if candidateURL(c) == "" {
			continue
		}
		area := candidateWidth(c) * candidateHeight(c)
		if !best.Exists() || area > bestArea {
			best = c
			bestArea = area
		}
	}
	return best
}

func candidateWidth(value gjson.Result) int {
	return cmp.Or(uintOf(value, "width"), uintOf(value, "config_width"))
}

func candidateHeight(value gjson.Result) int {
	return cmp.Or(uintOf(value, "height"), uintOf(value, "config_height"))
}

func v1StatsPrefix(item gjson.Result) string {
	if play := cmp.Or(uintOf(item, "play_count"), uintOf(item, "video_play_count"), uintOf(item, "view_count"),
		uintOf(item, "video_view_count"), uintOf(item, "ig_play_count"), uintOf(item, "fb_play_count")); play > 0 {
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
	return time.Time{}
}
