package main

import (
	"strings"
	"testing"
)

const sampleCarousel = `{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{
  "code":"DZWI_exgXz7","media_type":8,
  "user":{"username":"designcompass","full_name":"Design Compass","profile_pic_url":"https://cdn/pp.jpg"},
  "caption":{"text":"hello\nworld"},"like_count":226,"comment_count":5,
  "carousel_media":[
    {"media_type":1,"original_width":1080,"original_height":1350,"image_versions2":{"candidates":[{"url":"https://cdn/img1.jpg","width":1080,"height":1350}]}},
    {"media_type":2,"original_width":720,"original_height":1280,"image_versions2":{"candidates":[{"url":"https://cdn/vcover.jpg","width":720,"height":1280}]},"video_versions":[{"url":"https://cdn/vid.mp4"}]}
  ]
}}}}`

func TestParseCarousel(t *testing.T) {
	post, err := parseInstagramPost(sampleCarousel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Username != "designcompass" || post.FullName != "Design Compass" {
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
	_, err := parseInstagramPost(`{"data":{"xdt_shortcode_media":null}}`)
	if err == nil || err.Status != 404 {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestNormalizeCDNHost(t *testing.T) {
	cases := map[string]string{
		"https://instagram.fcps4-1.fna.fbcdn.net/v/x.mp4?oh=1&oe=2": "https://scontent.cdninstagram.com/v/x.mp4?oh=1&oe=2",
		"https://scontent-atl3-3.cdninstagram.com/v/x.mp4":          "https://scontent-atl3-3.cdninstagram.com/v/x.mp4",
		"":                      "",
		"https://example.com/x": "https://example.com/x",
	}
	for in, want := range cases {
		if got := normalizeCDNHost(in); got != want {
			t.Fatalf("normalizeCDNHost(%q)=%q want %q", in, got, want)
		}
	}
}

const sampleLoggedOut = `{"data":{"xig_polaris_media":{"__typename":"XIGPolarisVideoMedia","if_not_gated_logged_out":{
  "pk":"3929188310019024770","code":"DaHR_NCTSeC","taken_at":1782615823,
  "media_type":2,"product_type":"clips","like_count":13215,"comment_count":49,
  "caption":{"text":"初アリーナツアー 開催決定👑🌈✨\n\n#CUTIESTREET"},
  "original_height":1920,"original_width":1080,
  "user":{"pk":"67661243995","username":"cutie_street_","full_name":"CUTIE STREET","profile_pic_url":"https://cdn/pp.jpg"},
  "image_versions2":{"candidates":[{"url":"https://cdn/cover_big.jpg","width":1206,"height":2144},{"url":"https://cdn/cover_small.jpg","width":640,"height":1138}]},
  "video_versions":[{"type":101,"url":"https://cdn/video.mp4"}]
}}}}`

func TestParseLoggedOut(t *testing.T) {
	post, err := parseInstagramPost(sampleLoggedOut)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if post.Username != "cutie_street_" || post.FullName != "CUTIE STREET" {
		t.Errorf("user = %q / %q", post.Username, post.FullName)
	}
	if post.Shortcode != "DaHR_NCTSeC" {
		t.Errorf("shortcode = %q", post.Shortcode)
	}
	if !strings.Contains(post.Caption, "初アリーナツアー") {
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
