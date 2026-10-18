package main

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// GraphQL error code Instagram uses for per-client throttling.
const graphQLRateLimitedCode = 1675004

func parseInstagramPost(body string) (Post, *AppError) {
	root := gjson.Parse(body)
	data := root.Get("data")

	if it := data.Get("xdt_api__v1__media__shortcode__web_info.items.0"); present(it) {
		return parseV1(it)
	}
	// Errors arrive with data:null. field_exception (1675030) comes back for
	// gated, hidden and missing posts alike, so it is not proof of absence.
	if first := root.Get("errors.0"); first.Exists() {
		code, msg := first.Get("code").Int(), first.Get("message").String()
		cause := fmt.Errorf("graphql %d: %s", code, msg)
		if code == graphQLRateLimitedCode || strings.Contains(msg, "Please wait") {
			return Post{}, causedErr(429, errorCodeRateLimited, "Instagram rate limited the request", cause)
		}
		return Post{}, causedErr(502, errorCodeGraphQL, "Instagram could not load this post", cause)
	}
	// Only an explicitly empty items list proves absence; any other shape is schema drift.
	if items := data.Get("xdt_api__v1__media__shortcode__web_info.items"); items.IsArray() && len(items.Array()) == 0 {
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

	media := item.Get("carousel_media").Array()
	if len(media) == 0 {
		media = []gjson.Result{item}
	}
	// Dropping an unusable child would shift every later img_index, so let the next source try.
	var attachments []Attachment
	for _, child := range media {
		att, ok := parseV1Attachment(child)
		if !ok {
			return Post{}, igErr(502, errorCodeUpstream, "v1 media had an attachment without an image")
		}
		attachments = append(attachments, att)
	}

	return Post{
		Shortcode:   cmp.Or(item.Get("code").String(), item.Get("shortcode").String()),
		Username:    username,
		FullName:    fullName,
		ProfilePic:  user.Get("profile_pic_url").String(),
		IsVerified:  user.Get("is_verified").Type == gjson.True,
		Caption:     cmp.Or(item.Get("caption.text").String(), item.Get("caption_text").String()),
		StatsLine:   statsLine(v1StatsPrefix(item), countOf(item, "like_count"), countOf(item, "comment_count")),
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
	if video := bestVideoURL(item); video != "" && uintOf(item, "media_type") == 2 {
		return Attachment{Kind: "video", URL: video, Thumbnail: thumbnail, Width: w, Height: h}, true
	}
	return Attachment{Kind: "image", URL: thumbnail, Thumbnail: thumbnail, Width: w, Height: h}, true
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

func countOf(value gjson.Result, path string) int {
	if !present(value.Get(path)) {
		return -1
	}
	return uintOf(value, path)
}

func unixTime(seconds int64) time.Time {
	if seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	return time.Time{}
}
