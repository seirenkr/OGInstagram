package main

import (
	"strings"
	"testing"
)

const sampleCarousel = `{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{
  "code":"DaQanvRgP-q","media_type":8,
  "user":{"username":"instagram","full_name":"Instagram","profile_pic_url":"https://cdn/pp.jpg"},
  "caption":{"text":"hello\nworld"},"like_count":226,"comment_count":5,
  "carousel_media":[
    {"media_type":1,"original_width":1080,"original_height":1350,"image_versions2":{"candidates":[{"url":"https://cdn/img1.jpg","width":1080,"height":1350}]}},
    {"media_type":2,"original_width":720,"original_height":1280,"image_versions2":{"candidates":[{"url":"https://cdn/vcover.jpg","width":720,"height":1280}]},"video_versions":[{"url":"https://cdn/vid.mp4"}]}
  ]
}}}}`

func TestParseGraphQLErrors(t *testing.T) {
	for _, tc := range []struct {
		body   string
		hidden bool
	}{
		{`{"data":{"xig_polaris_media":null}}`, true},
		{`{"data":{"xig_polaris_media":{"pk":"1","if_not_gated_logged_out":null,"gating_ruling":{"gating_type":3}}},"errors":[{"message":"A server error field_exception occured."}]}`, true},
		{`{"data":null,"errors":[{"message":"Rate limit exceeded","code":1675004}]}`, false},
		{`{"data":{"xdt_shortcode_media":null}}`, false},
	} {
		if _, err := parseInstagramPost(tc.body); err == nil || err.Code != errorCodeGraphQL || graphQLHidden(tc.body) != tc.hidden {
			t.Errorf("%s: got %v, hidden %v; want a graphql error, hidden %v", tc.body, err, graphQLHidden(tc.body), tc.hidden)
		}
	}
	if !isTransient(errorCodeGraphQL) {
		t.Fatal("graphql_error must stay transient")
	}
}

func TestParseV1MissingMedia(t *testing.T) {
	post, err := parseInstagramPost(`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"code":"V","media_type":2,"comment_count":3,` +
		`"user":{"username":"u"},"image_versions2":{"candidates":[{"url":"https://cdn/cover.jpg"}]}}}}}`)
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

const sampleLoggedOut = `{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{
  "pk":"3932567351408976603","code":"DaTSSukB-Lb","taken_at":1782615823,
  "media_type":2,"product_type":"clips","like_count":13215,"comment_count":49,
  "caption":{"text":"this glow >>>\n\n#InTheMoment"},
  "original_height":1920,"original_width":1080,
  "user":{"pk":"25025320","username":"instagram","full_name":"Instagram","profile_pic_url":"https://cdn/pp.jpg"},
  "image_versions2":{"candidates":[{"url":"https://cdn/cover_big.jpg","width":1206,"height":2144},{"url":"https://cdn/cover_small.jpg","width":640,"height":1138}]},
  "video_versions":[{"type":101,"url":"https://cdn/video.mp4"}]
}}}}`

func TestParseLoggedOut(t *testing.T) {
	post, err := parseInstagramPost(sampleLoggedOut)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if post.Username != "instagram" || post.FullName != "Instagram" || post.Shortcode != "DaTSSukB-Lb" ||
		!strings.Contains(post.Caption, "#InTheMoment") || post.StatsLine != "❤️ 13,215  💬 49" {
		t.Fatalf("post fields wrong: %+v", post)
	}
	if len(post.Attachments) != 1 || post.Attachments[0].Kind != "video" || post.Attachments[0].URL != "https://cdn/video.mp4" ||
		post.Attachments[0].Thumbnail != "https://cdn/cover_big.jpg" || post.Attachments[0].Width != 1080 || post.Attachments[0].Height != 1920 {
		t.Fatalf("video attachment wrong: %+v", post.Attachments)
	}

	carousel, err := parseInstagramPost(sampleCarousel)
	if err != nil || len(carousel.Attachments) != 2 || carousel.Attachments[0].Kind != "image" ||
		carousel.Attachments[0].URL != "https://cdn/img1.jpg" || carousel.Attachments[1].Kind != "video" ||
		carousel.Attachments[1].URL != "https://cdn/vid.mp4" || carousel.StatsLine != "❤️ 226  💬 5" {
		t.Fatalf("carousel = %+v, %v", carousel, err)
	}

	sizeless, err := parseInstagramPost(`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{` +
		`"code":"S1","media_type":1,"like_count":3,"like_and_view_counts_disabled":true,"comment_count":2,` +
		`"user":{"username":"u"},"image_versions2":{"candidates":[{"url":"https://cdn/orig.jpg"},{"url":"https://cdn/thumb.jpg"}]}}}}}`)
	if err != nil || sizeless.Attachments[0].URL != "https://cdn/orig.jpg" || sizeless.StatsLine != "💬 2" {
		t.Fatalf("sizeless/hidden-likes post = %+v, %v", sizeless, err)
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
