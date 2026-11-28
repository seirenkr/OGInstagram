package main

import (
	"cmp"
	"fmt"
	"time"

	"github.com/tidwall/gjson"
)

const graphQLRateLimitedCode = 1675004

const (
	loggedOutMediaPath = "data.xig_polaris_media"
	loggedOutItemPath  = loggedOutMediaPath + ".if_not_gated_logged_out"
)

type graphQLGating struct {
	gatingType int
	title      string
	desc       string
}

func graphQLHidden(body string) bool {
	return gjson.Get(body, loggedOutMediaPath).Exists() && !present(gjson.Get(body, loggedOutItemPath))
}

func parseGraphQLGating(body string) graphQLGating {
	r := gjson.Get(body, loggedOutMediaPath+".gating_ruling")
	return graphQLGating{
		gatingType: int(r.Get("gating_type").Int()),
		title:      r.Get("title").String(),
		desc:       r.Get("description").String(),
	}
}

func parseInstagramPost(body string) (Post, *AppError) {
	if it := gjson.Get(body, loggedOutItemPath); present(it) {
		return parseV1(it)
	}
	if graphQLHidden(body) {
		gating := gjson.Get(body, loggedOutMediaPath+".gating_ruling.gating_type")
		return Post{}, causedErr(502, errorCodeGraphQL, "Instagram could not load this post", fmt.Errorf("graphql withheld the media: gating_type %s", cmp.Or(gating.Raw, "none")))
	}
	if first := gjson.Get(body, "errors.0"); first.Exists() {
		return Post{}, causedErr(502, errorCodeGraphQL, "Instagram could not load this post", fmt.Errorf("graphql %d: %s", first.Get("code").Int(), first.Get("message").String()))
	}
	return Post{}, igErr(502, errorCodeGraphQL, "Instagram response did not include media")
}

func parseV1(item gjson.Result) (Post, *AppError) {
	user := item.Get("user")
	if !user.Exists() {
		return Post{}, igErr(502, errorCodeUpstream, "missing user")
	}

	media := item.Get("carousel_media").Array()
	if len(media) == 0 {
		media = []gjson.Result{item}
	}
	var attachments []Attachment
	for _, child := range media {
		att, ok := parseV1Attachment(child)
		if !ok {
			return Post{}, igErr(502, errorCodeUpstream, "v1 media had an attachment without an image")
		}
		attachments = append(attachments, att)
	}

	likes := countOf(item, "like_count")
	if item.Get("like_and_view_counts_disabled").Bool() {
		likes = -1
	}

	return Post{
		Shortcode:   item.Get("code").String(),
		Username:    user.Get("username").String(),
		FullName:    user.Get("full_name").String(),
		ProfilePic:  user.Get("profile_pic_url").String(),
		IsVerified:  user.Get("is_verified").Type == gjson.True,
		Caption:     item.Get("caption.text").String(),
		StatsLine:   statsLine("", likes, countOf(item, "comment_count")),
		Attachments: attachments,
		CreatedAt:   unixTime(item.Get("taken_at").Int()),
	}, nil
}

func parseV1Attachment(item gjson.Result) (Attachment, bool) {
	thumbnail := cmp.Or(bestCandidateURL(item.Get("image_versions2.candidates")), item.Get("display_uri").String())
	if thumbnail == "" {
		return Attachment{}, false
	}
	w, h := mediaSize(item)
	if video := bestCandidateURL(item.Get("video_versions")); video != "" && uintOf(item, "media_type") == 2 {
		return Attachment{Kind: "video", URL: video, Thumbnail: thumbnail, Width: w, Height: h}, true
	}
	return Attachment{Kind: "image", URL: thumbnail, Thumbnail: thumbnail, Width: w, Height: h}, true
}

func bestCandidateURL(value gjson.Result) string {
	return candidateURL(bestCandidate(value))
}

func candidateURL(c gjson.Result) string {
	return cmp.Or(c.Get("url").String(), c.Get("src").String())
}

func mediaSize(v gjson.Result) (int, int) {
	c := bestImageCandidate(v)
	return cmp.Or(uintOf(v, "original_width"), uintOf(v, "width"), candidateWidth(c)),
		cmp.Or(uintOf(v, "original_height"), uintOf(v, "height"), candidateHeight(c))
}

func bestImageCandidate(value gjson.Result) gjson.Result {
	return bestCandidate(value.Get("image_versions2.candidates"))
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
