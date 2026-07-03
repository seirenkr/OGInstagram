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

func TestRaceEmbedFirst(t *testing.T) {
	// Embed answers first: GraphQL never launches (no proxy budget spent).
	gqlCalled := false
	p, err := raceEmbedFirst(
		func() (Post, *AppError) { return Post{Shortcode: "a"}, nil },
		func() (Post, *AppError) { gqlCalled = true; return Post{}, igErr(502, reasonGraphql, "x") },
	)
	if err != nil || p.Shortcode != "a" {
		t.Fatalf("embed win: post=%+v err=%+v", p, err)
	}
	if gqlCalled {
		t.Error("gql should not launch when embed answers first")
	}

	// Embed fails: GraphQL launches immediately, without the hedge wait.
	start := time.Now()
	p, err = raceEmbedFirst(
		func() (Post, *AppError) { return Post{}, igErr(502, reasonGraphql, "embed down") },
		func() (Post, *AppError) { return Post{Shortcode: "b"}, nil },
	)
	if err != nil || p.Shortcode != "b" {
		t.Fatalf("gql fallback: post=%+v err=%+v", p, err)
	}
	if time.Since(start) > fetchHedgeDelay/2 {
		t.Error("gql should launch on embed failure, not after the hedge delay")
	}

	// Both fail: the permanent error (real 404) beats the transient one.
	_, err = raceEmbedFirst(
		func() (Post, *AppError) { return Post{}, igErr(502, reasonGraphql, "transient") },
		func() (Post, *AppError) { return Post{}, igErr(404, reasonMediaNotFound, "gone") },
	)
	if err == nil || err.Reason != reasonMediaNotFound {
		t.Fatalf("want permanent error to win, got %+v", err)
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
