package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestShortcodeTime(t *testing.T) {

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

	if _, ok := parseOembedPost("x", `{"status":"fail","title":"게시물을 사용할 수 없음","message":"삭제되었을 수 있습니다."}`); ok {
		t.Error("fail payload should not parse")
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

func TestStagedFetch(t *testing.T) {
	ctx := context.Background()
	str := func(v string, err *AppError) func(context.Context) (string, *AppError) {
		return func(context.Context) (string, *AppError) { return v, err }
	}

	hedgeCalled := false
	v, _, err := stagedFetch(ctx, stagedSource[string]{fetch: str("a", nil)},
		stagedSource[string]{after: time.Second, fetch: func(context.Context) (string, *AppError) { hedgeCalled = true; return "b", nil }})
	if v != "a" || err != nil || hedgeCalled {
		t.Fatalf("primary win = %q %v hedge=%v", v, err, hedgeCalled)
	}

	started := time.Now()
	v, persist, err := stagedFetch(ctx, stagedSource[string]{fetch: str("", ephemeralErr(502, errorCodeConnection, "x"))},
		stagedSource[string]{after: time.Second, persist: true, fetch: str("b", nil)})
	if v != "b" || !persist || err != nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("failure did not pull the hedge in early: %q %v %v %s", v, persist, err, time.Since(started))
	}

	// A permanent error outranks a later transient one.
	_, _, err = stagedFetch(ctx, stagedSource[string]{fetch: str("", igErr(404, errorCodeNotFound, "x"))},
		stagedSource[string]{after: time.Second, fetch: str("", ephemeralErr(502, errorCodeConnection, "x"))})
	if err == nil || err.Code != errorCodeNotFound {
		t.Fatalf("preferred error = %v", err)
	}

	_, _, err = stagedFetch(ctx, stagedSource[string]{fetch: str("", ephemeralErr(502, errorCodeConnection, "x"))})
	if err == nil || err.Code != errorCodeConnection {
		t.Fatalf("single source error = %v", err)
	}
}

func TestFetchViaProxyBlamesOnlyUpstreamFailures(t *testing.T) {
	respond := func(status int, body string) roundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			return mediaTestResponse(r, status, nil, []byte(body)), nil
		}
	}
	for _, tt := range []struct {
		name    string
		rt      roundTripFunc
		code    string
		rotated bool
	}{
		{"429", respond(429, "{}"), errorCodeRateLimited, true},
		{"refused", func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }, errorCodeConnection, true},
		{"deadline", func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }, errorCodeConnection, false},
		{"budget", func(*http.Request) (*http.Response, error) {
			return nil, &proxyBudgetError{code: errorCodeBudgetExhausted}
		}, errorCodeBudgetExhausted, false},
		{"ok", respond(200, "{}"), "", false},
	} {
		client := &http.Client{Transport: tt.rt}
		s := &Session{client: client, windowStart: time.Now()}
		a := &App{pool: &SessionPool{sessions: []*Session{s}, budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}}
		timeout := 5 * time.Second
		if tt.name == "deadline" {
			timeout = 50 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, _, err := a.fetchViaProxy(ctx, fetchSpec{method: "GET", url: "https://www.instagram.com/x", interpret: instagramJSON})
		cancel()
		code := ""
		if err != nil {
			code = err.Code
		}
		rotated := s.getClient() != client && s.cooldownUntil.After(time.Now())
		if code != tt.code || rotated != tt.rotated || (tt.code == "" && !s.hasEWMA) {
			t.Errorf("%s: code=%q rotated=%v ewma=%v, want %q rotated=%v", tt.name, code, rotated, s.hasEWMA, tt.code, tt.rotated)
		}
	}
}
