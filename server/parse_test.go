package main

import (
	"strings"
	"testing"
)

const sampleCarousel = `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{
  "code":"DaQanvRgP-q","media_type":8,
  "user":{"username":"instagram","full_name":"Instagram","profile_pic_url":"https://cdn/pp.jpg"},
  "caption":{"text":"hello\nworld"},"like_count":226,"comment_count":5,
  "carousel_media":[
    {"media_type":1,"original_width":1080,"original_height":1350,"image_versions2":{"candidates":[{"url":"https://cdn/img1.jpg","width":1080,"height":1350}]}},
    {"media_type":2,"original_width":720,"original_height":1280,"image_versions2":{"candidates":[{"url":"https://cdn/vcover.jpg","width":720,"height":1280}]},"video_versions":[{"url":"https://cdn/vid.mp4"}]}
  ]
}]}}}`

func TestParseCarousel(t *testing.T) {
	post, err := parseInstagramPost(sampleCarousel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Username != "instagram" || post.FullName != "Instagram" {
		t.Fatalf("owner mismatch: %q / %q", post.Username, post.FullName)
	}
	if post.Caption != "hello\nworld" {
		t.Fatalf("caption mismatch: %q", post.Caption)
	}
	if len(post.Attachments) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(post.Attachments))
	}
	img := post.Attachments[0]
	if img.Kind != "image" || img.URL != "https://cdn/img1.jpg" {
		t.Fatalf("image attachment wrong: %+v", img)
	}
	if img.Width != 1080 || img.Height != 1350 {
		t.Fatalf("image dims wrong: %dx%d", img.Width, img.Height)
	}
	vid := post.Attachments[1]
	if vid.Kind != "video" || vid.URL != "https://cdn/vid.mp4" || vid.Thumbnail != "https://cdn/vcover.jpg" {
		t.Fatalf("video attachment wrong: %+v", vid)
	}
	if post.StatsLine != "❤️ 226  \U0001f4ac 5" {
		t.Fatalf("stats line wrong: %q", post.StatsLine)
	}
}

func TestParseNotFound(t *testing.T) {
	_, err := parseInstagramPost(`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[]}}}`)
	if err == nil || err.Status != 404 {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestParseGraphQLErrors(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"data":null,"errors":[{"message":"A server error field_exception occured.","code":1675030}]}`, errorCodeGraphQL},
		{`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[]}}}`, errorCodeMediaNotFound},
		{`{"data":{"xdt_shortcode_media":null}}`, errorCodeGraphQL},
	} {
		if _, err := parseInstagramPost(tc.body); err == nil || err.Code != tc.code {
			t.Errorf("%s: got %v, want %s", tc.body, err, tc.code)
		}
	}
	// A GraphQL field_exception is not proof of absence: it must not be cached as permanent.
	if !isTransient(errorCodeGraphQL) {
		t.Fatal("graphql_error must stay transient")
	}
}

func TestParseV1MissingMedia(t *testing.T) {
	post, err := parseInstagramPost(`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"V","media_type":2,"comment_count":3,` +
		`"user":{"username":"u"},"image_versions2":{"candidates":[{"url":"https://cdn/cover.jpg"}]}}]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if a := post.Attachments[0]; a.Kind != "image" || a.URL != "https://cdn/cover.jpg" || post.StatsLine != "💬 3" {
		t.Fatalf("video without URL: %+v, stats %q", a, post.StatsLine)
	}
	imageless := strings.Replace(sampleCarousel, `"url":"https://cdn/img1.jpg"`, `"url":""`, 1)
	if _, err := parseInstagramPost(imageless); err == nil || err.Code != errorCodeUpstream {
		t.Fatalf("carousel child without image: got %v, want upstream error", err)
	}
}

func TestParseProfileShapes(t *testing.T) {
	for _, body := range []string{`{}`, `{"graphql":{"user":{}}}`, `{"data":{"xig_user_by_username":{"pk":"1"}}}`} {
		if _, err := parseProfileRoot(body); err == nil || !isTransient(err.Code) || err.Final {
			t.Errorf("parseProfileRoot(%s) = %v, want transient", body, err)
		}
	}
	if _, err := parseEmbedProfile(wrapEmbed(`{"context":{"user_name":"nasa"}}`)); err == nil || !isTransient(err.Code) {
		t.Errorf("embed profile without username = %v, want transient", err)
	}
	// null: no such account (with 1675030) or a renamed or age-restricted one.
	for _, body := range []string{`{"data":{"xig_user_by_username":null},"errors":[{"code":1675030}]}`, `{"data":{"xig_user_by_username":null}}`} {
		if _, err := parseProfileRoot(body); err == nil || err.Code != errorCodeNotFound || !err.Final {
			t.Errorf("parseProfileRoot(%s) = %v, want a final not_found", body, err)
		}
	}
	p, err := parseProfileRoot(`{"data":{"xig_user_by_username":{"username":"u","biography":"b","follower_count":7,"is_private":true}}}`)
	if err != nil || p.Biography != "b" || p.FollowerCount != 7 || !p.IsPrivate || p.MediaCount != unknownCount || profileStatsLine(p) != "👤 7" {
		t.Fatalf("profile = %+v, %v; want the account without a post count", p, err)
	}
	gallery := parseProfilePosts(`{"data":{"xig_user_by_username":{"polaris_ordered_timeline_connection":{"edges":[` +
		`{"node":{"display_uri":"https://cdn/1.jpg"}},{"node":{}},{"node":{"display_uri":"https://cdn/2.jpg"}}]}}}}`)
	if len(gallery.RecentMedia) != 2 || gallery.RecentMedia[1].Thumbnail != "https://cdn/2.jpg" {
		t.Fatalf("gallery = %+v; want the two thumbnails", gallery.RecentMedia)
	}
}

func TestNormalizeCDNHost(t *testing.T) {
	cases := map[string]string{
		"https://instagram.fcps4-1.fna.fbcdn.net/v/x.mp4?oh=1&oe=2": "https://scontent.cdninstagram.com/v/x.mp4?oh=1&oe=2",
		"https://scontent-atl3-3.cdninstagram.com/v/x.mp4":          "https://scontent.cdninstagram.com/v/x.mp4",
		"https://scontent.cdninstagram.com/v/x.jpg":                 "https://scontent.cdninstagram.com/v/x.jpg",
		"":                      "",
		"https://example.com/x": "https://example.com/x",
	}
	for in, want := range cases {
		if got := normalizeCDNHost(in); got != want {
			t.Fatalf("normalizeCDNHost(%q)=%q want %q", in, got, want)
		}
	}
}

const sampleLoggedOut = `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{
  "pk":"3932567351408976603","code":"DaTSSukB-Lb","taken_at":1782615823,
  "media_type":2,"product_type":"clips","like_count":13215,"comment_count":49,
  "caption":{"text":"this glow >>>\n\n#InTheMoment"},
  "original_height":1920,"original_width":1080,
  "user":{"pk":"25025320","username":"instagram","full_name":"Instagram","profile_pic_url":"https://cdn/pp.jpg"},
  "image_versions2":{"candidates":[{"url":"https://cdn/cover_big.jpg","width":1206,"height":2144},{"url":"https://cdn/cover_small.jpg","width":640,"height":1138}]},
  "video_versions":[{"type":101,"url":"https://cdn/video.mp4"}]
}]}}}`

func TestParseLoggedOut(t *testing.T) {
	post, err := parseInstagramPost(sampleLoggedOut)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if post.Username != "instagram" || post.FullName != "Instagram" {
		t.Errorf("user = %q / %q", post.Username, post.FullName)
	}
	if post.Shortcode != "DaTSSukB-Lb" {
		t.Errorf("shortcode = %q", post.Shortcode)
	}
	if !strings.Contains(post.Caption, "#InTheMoment") {
		t.Errorf("caption = %q", post.Caption)
	}
	if post.StatsLine != "❤️ 13,215  💬 49" {
		t.Errorf("stats = %q", post.StatsLine)
	}
	if len(post.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(post.Attachments))
	}
	a := post.Attachments[0]
	if a.Kind != "video" || a.URL != "https://cdn/video.mp4" {
		t.Errorf("attachment = %+v", a)
	}
	if a.Thumbnail != "https://cdn/cover_big.jpg" {
		t.Errorf("thumbnail = %q (want largest candidate)", a.Thumbnail)
	}
	if a.Width != 1080 || a.Height != 1920 {
		t.Errorf("dims = %dx%d, want 1080x1920 (from original_width/height)", a.Width, a.Height)
	}
}

func TestVerificationFromAuthorJSON(t *testing.T) {
	const v = `,"is_verified":true`
	post, err1 := parseInstagramPost(strings.Replace(sampleLoggedOut, `"full_name":"Instagram"`, `"full_name":"Instagram"`+v, 1))
	embed, err2 := parseEmbedPost(wrapEmbed(`{"gql_data":{"shortcode_media":{"shortcode":"ABC","is_video":false,"owner":{"username":"nasa"` + v + `},"display_url":"https://cdn/p.jpg"}}}`))
	root, err3 := parseProfileRoot(`{"data":{"xig_user_by_username":{"username":"nasa"` + v + `}}}`)
	profile, err4 := parseEmbedProfile(wrapEmbed(`{"context":{"username":"instagram"` + v + `,"graphql_media":[]}}`))
	quoted, err5 := parseProfileRoot(`{"data":{"xig_user_by_username":{"username":"nasa","is_verified":"true"}}}`)
	for name, ok := range map[string]bool{
		"post GraphQL user":        err1 == nil && post.IsVerified,
		"post embed owner":         err2 == nil && embed.IsVerified,
		"profile RootContent user": err3 == nil && root.IsVerified,
		"profile embed context":    err4 == nil && profile.IsVerified,
		"only a JSON true counts":  err5 == nil && !quoted.IsVerified,
	} {
		if !ok {
			t.Errorf("%s: is_verified not read", name)
		}
	}
}
