package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestShortcodeTime(t *testing.T) {
	// world_record_egg, posted 2019-01-04: snowflake decode is ms-exact.
	want := time.Date(2019, 1, 4, 17, 5, 45, 106e6, time.UTC)
	if got := shortcodeTime("BsOGulcndj-"); !got.Equal(want) {
		t.Errorf("shortcodeTime(BsOGulcndj-) = %v, want %v", got, want)
	}
	for _, sc := range []string{"", "has space", "AAAAAAAAAAAAAAAAAAAAAAAA"} {
		if got := shortcodeTime(sc); !got.IsZero() {
			t.Errorf("shortcodeTime(%q) = %v, want zero", sc, got)
		}
	}
}

func TestFlagOversizedVideos(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "big") {
			w.Header().Set("Content-Length", strconv.FormatInt(int64(maxInlineVideoBytes)+1, 10))
		}
	}))
	defer ts.Close()
	a := &App{direct: ts.Client()}
	post := Post{Shortcode: "X", Attachments: []Attachment{
		{Kind: "image", URL: ts.URL + "/img.jpg"},
		{Kind: "video", URL: ts.URL + "/big.mp4"},
	}}
	a.flagOversizedVideos(&post)
	if !post.Attachments[1].OversizedInline {
		t.Error("oversized video should be flagged")
	}
	if post.Attachments[0].OversizedInline {
		t.Error("image must not be size-flagged")
	}
}

func TestHedgedRace(t *testing.T) {
	one := func(f func() (Post, *AppError)) []attempt[Post] { return []attempt[Post]{f} }

	// Initial attempt answers first: the hedge never launches (no proxy budget spent).
	hedgeCalled := false
	p, err := hedgedRace(
		one(func() (Post, *AppError) { return Post{Shortcode: "a"}, nil }),
		func() []attempt[Post] {
			hedgeCalled = true
			return one(func() (Post, *AppError) { return Post{}, igErr(502, reasonGraphql, "x") })
		},
	)
	if err != nil || p.Shortcode != "a" {
		t.Fatalf("initial win: post=%+v err=%+v", p, err)
	}
	if hedgeCalled {
		t.Error("hedge should not launch when the initial attempt answers first")
	}

	// Initial attempt fails: the hedge launches immediately, without the hedge wait.
	start := time.Now()
	p, err = hedgedRace(
		one(func() (Post, *AppError) { return Post{}, igErr(502, reasonGraphql, "embed down") }),
		func() []attempt[Post] { return one(func() (Post, *AppError) { return Post{Shortcode: "b"}, nil }) },
	)
	if err != nil || p.Shortcode != "b" {
		t.Fatalf("hedge fallback: post=%+v err=%+v", p, err)
	}
	if time.Since(start) > fetchHedgeDelay/2 {
		t.Error("hedge should launch on initial failure, not after the hedge delay")
	}

	// Both fail: the permanent error (real 404) beats the transient one.
	_, err = hedgedRace(
		one(func() (Post, *AppError) { return Post{}, igErr(502, reasonGraphql, "transient") }),
		func() []attempt[Post] {
			return one(func() (Post, *AppError) { return Post{}, igErr(404, reasonMediaNotFound, "gone") })
		},
	)
	if err == nil || err.Reason != reasonMediaNotFound {
		t.Fatalf("want permanent error to win, got %+v", err)
	}
}

func TestParseOembedPost(t *testing.T) {
	body := `{
		"version": "1.0",
		"title": "time to create",
		"author_name": "instagram",
		"author_url": "https://www.instagram.com/instagram",
		"author_id": 25025320,
		"media_id": "3928250036051888465_25025320",
		"type": "rich",
		"html": "<a href=\"https://www.instagram.com/p/DaD8phTyclR/\" target=\"_blank\">A post shared by Instagram (@instagram)</a>",
		"thumbnail_url": "https://scontent-gmp1-1.cdninstagram.com/v/t51.82787-15/x.jpg?oe=6A4FBF9C",
		"thumbnail_width": 640,
		"thumbnail_height": 800
	}`
	p, ok := parseOembedPost("DaD8phTyclR", body)
	if !ok {
		t.Fatal("oembed success payload should parse")
	}
	if p.Username != "instagram" || p.FullName != "Instagram" || p.OwnerID != "25025320" {
		t.Errorf("author fields: %+v", p)
	}
	if p.Shortcode != "DaD8phTyclR" || !strings.Contains(p.Caption, "time to create") {
		t.Errorf("post fields: %+v", p)
	}
	att := p.Attachments[0]
	if att.ID != "3928250036051888465" || att.Kind != "image" || att.Width != 640 || att.Height != 800 {
		t.Errorf("attachment: %+v", att)
	}
	if att.URL == "" || att.URL != att.Thumbnail {
		t.Errorf("thumbnail-only attachment expected: %+v", att)
	}

	// A fail payload must not parse into a post.
	if _, ok := parseOembedPost("x", `{"status":"fail","title":"게시물을 사용할 수 없음","message":"삭제되었을 수 있습니다."}`); ok {
		t.Error("fail payload should not parse")
	}
}

func TestNewLSDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := newLSD()
		if len(s) < 23 || len(s) > 27 {
			t.Fatalf("lsd length %d out of range [23,27]: %q", len(s), s)
		}
		for _, c := range s {
			ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !ok {
				t.Fatalf("lsd has non-alphanumeric char %q in %q", c, s)
			}
		}
		seen[s] = true
	}
	if len(seen) < 90 {
		t.Errorf("lsd not random enough: %d unique of 100", len(seen))
	}
}

func TestShortcodePK(t *testing.T) {
	if got := shortcodePK("DaEd82_pQ40"); got == nil || got.String() != "3928396500541181492" {
		t.Errorf("shortcodePK(DaEd82_pQ40) = %v, want 3928396500541181492", got)
	}
	if got := shortcodePK("has space"); got != nil {
		t.Errorf("invalid char should yield nil, got %v", got)
	}
}

func TestWebLoggedOutSpec(t *testing.T) {
	spec := webLoggedOutSpec("DaEd82_pQ40")
	if spec.method != http.MethodPost {
		t.Errorf("method = %q, want POST", spec.method)
	}
	if spec.url != "https://www.instagram.com/graphql/query" {
		t.Errorf("url = %q", spec.url)
	}
	if spec.headers["X-FB-Friendly-Name"] != "PolarisPostRootQuery" {
		t.Errorf("friendly name = %q", spec.headers["X-FB-Friendly-Name"])
	}
	// A modern-browser UA gets an HTML login shell; the minimal UA is required.
	if spec.headers["User-Agent"] != "Mozilla/5.0" {
		t.Errorf("user-agent = %q, want minimal Mozilla/5.0", spec.headers["User-Agent"])
	}
	vals, err := url.ParseQuery(spec.body)
	if err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if vals.Get("doc_id") != instagramWebLoggedOutDocID {
		t.Errorf("doc_id = %q, want %q", vals.Get("doc_id"), instagramWebLoggedOutDocID)
	}
	if lsd := vals.Get("lsd"); lsd == "" || lsd != spec.headers["X-FB-LSD"] {
		t.Errorf("lsd mismatch: body=%q header=%q", lsd, spec.headers["X-FB-LSD"])
	}
	v := vals.Get("variables")
	if !strings.Contains(v, `"shortcode":"DaEd82_pQ40"`) {
		t.Errorf("variables should hold shortcode, got %q", v)
	}
}
