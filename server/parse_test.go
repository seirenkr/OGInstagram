package main

import "testing"

const sampleXDT = `{"data":{"xdt_shortcode_media":{
  "__typename":"XDTGraphSidecar",
  "shortcode":"DZWI_exgXz7",
  "taken_at_timestamp":1700000000,
  "edge_media_preview_like":{"count":226},
  "edge_media_to_parent_comment":{"count":5},
  "edge_media_to_caption":{"edges":[{"node":{"text":"hello\nworld"}}]},
  "owner":{"username":"designcompass","full_name":"Design Compass","profile_pic_url":"https://cdn/pp.jpg"},
  "edge_sidecar_to_children":{"edges":[
    {"node":{"__typename":"XDTGraphImage","is_video":false,"display_url":"https://cdn/img1.jpg","display_resources":[{"src":"https://cdn/img1_small.jpg","config_width":640,"config_height":800},{"src":"https://cdn/img1_big.jpg","config_width":1080,"config_height":1350}],"dimensions":{"width":1080,"height":1350}}},
    {"node":{"__typename":"XDTGraphVideo","is_video":true,"display_url":"https://cdn/vcover.jpg","video_url":"https://cdn/vid.mp4","dimensions":{"width":720,"height":1280}}}
  ]}
}}}`

func TestParseXDTSidecar(t *testing.T) {
	post, err := parseInstagramPost(sampleXDT)
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

func TestParseQueryHashShape(t *testing.T) {
	body := `{"data":{"shortcode_media":{"__typename":"GraphImage","shortcode":"X","owner":{"username":"u"},"display_url":"https://c/i.jpg","dimensions":{"width":10,"height":20},"edge_media_preview_like":{"count":0},"edge_media_to_parent_comment":{"count":0}}}}`
	post, err := parseInstagramPost(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(post.Attachments) != 1 || post.Attachments[0].URL != "https://c/i.jpg" {
		t.Fatalf("attachment wrong: %+v", post.Attachments)
	}
	if post.Username != "u" || post.FullName != "u" {
		t.Fatalf("owner fallback wrong: %q/%q", post.Username, post.FullName)
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
