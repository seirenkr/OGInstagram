package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
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
	if spec.url != "https://i.instagram.com/graphql/query" || !spec.expectJSON {
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
	if vals.Has("server_timestamps") {
		t.Error("unused server timestamps should not be requested")
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
		{"deadline", func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }, errorCodeConnection, true},
		{"budget", func(*http.Request) (*http.Response, error) {
			return nil, &proxyBudgetError{code: errorCodeBudgetExhausted}
		}, errorCodeBudgetExhausted, false},
		{"ok", respond(200, "{}"), "", false},
	} {
		client := &http.Client{Transport: tt.rt}
		s := &Session{client: client}
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
		if code != tt.code || rotated != tt.rotated || (tt.code == "" && s.rttAt.IsZero()) || s.pending != 0 {
			t.Errorf("%s: code=%q rotated=%v measured=%v pending=%d, want %q rotated=%v", tt.name, code, rotated, !s.rttAt.IsZero(), s.pending, tt.code, tt.rotated)
		}
	}
}

func TestLogOutboundSurvivesUnparsableURL(t *testing.T) {
	logOutbound(context.Background(), "op", "direct", http.MethodGet, "https://example.com/p#%zz", time.Now(), 0, 0,
		causedErr(502, errorCodeConnection, "boom", errors.New("boom")), false)
}

func TestHelperCaptchaCooldownPreservesInstagramAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		helperCalls, instagramCalls := 0, 0
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			status := http.StatusOK
			if req.URL.Host == "helper.example" {
				helperCalls++
				if helperCalls == 1 {
					status = http.StatusUnprocessableEntity
				}
			} else {
				instagramCalls++
			}
			return mediaTestResponse(req, status, nil, []byte(`{}`)), nil
		})}
		pool := newTestPool("shared")
		s := pool.sessions[0]
		s.client = client
		a := &App{pool: pool}
		helper := fetchSpec{method: http.MethodPost, url: "https://helper.example/convert", interpret: func(status int, raw []byte) *AppError {
			if status == http.StatusUnprocessableEntity {
				return igErr(http.StatusBadGateway, errorCodeCaptchaRequired, "verification required")
			}
			return statusOnly(status, raw)
		}}
		_, _, err := a.fetchViaProxy(context.Background(), helper)
		if err == nil || err.Code != errorCodeCaptchaRequired || helperCalls != 1 {
			t.Fatalf("initial challenge = %v, helper calls = %d", err, helperCalls)
		}
		if s.getClient() != client || !s.cooldownUntil.IsZero() || s.pending != 0 ||
			!s.helperCooldownUntil.Equal(time.Now().Add(helperCaptchaCooldown)) {
			t.Fatal("challenge should pause only helper requests on the same connection")
		}
		assertPaused := func() {
			t.Helper()
			status, _, err := a.fetchViaProxy(context.Background(), helper)
			if status != 0 || err == nil || err.Status != http.StatusServiceUnavailable ||
				err.Code != errorCodeCaptchaRequired || !err.Ephemeral || helperCalls != 1 || s.pending != 0 {
				t.Fatalf("paused helper sent a request or returned the wrong error: status=%d err=%v calls=%d", status, err, helperCalls)
			}
		}
		assertPaused()
		_, _, err = a.fetchViaProxy(context.Background(), fetchSpec{method: http.MethodGet, url: instagramGraphQLOrigin + "/graphql/query", interpret: statusOnly})
		if err != nil || instagramCalls != 1 || s.getClient() != client {
			t.Fatalf("Instagram stopped using the shared connection: err=%v calls=%d", err, instagramCalls)
		}
		time.Sleep(helperCaptchaCooldown - time.Nanosecond)
		assertPaused()
		time.Sleep(time.Nanosecond)
		_, _, err = a.fetchViaProxy(context.Background(), helper)
		if err != nil || helperCalls != 2 {
			t.Fatalf("helper did not resume after cooldown: err=%v calls=%d", err, helperCalls)
		}
	})
}

func TestHelperCaptchaCooldownUsesUnaffectedSession(t *testing.T) {
	pool := newTestPool("challenged", "ready")
	pool.sessions[0].helperCooldownUntil = time.Now().Add(helperCaptchaCooldown)
	pool.sessions[0].client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("helper used a challenged session")
		return nil, nil
	})}
	calls := 0
	pool.sessions[1].client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return mediaTestResponse(req, http.StatusOK, nil, []byte(`{}`)), nil
	})}
	a := &App{pool: pool}
	_, _, err := a.fetchViaProxy(context.Background(), fetchSpec{method: http.MethodGet, url: "https://helper.example/convert", interpret: statusOnly})
	if err != nil || calls != 1 {
		t.Fatalf("unaffected helper session was unavailable: err=%v calls=%d", err, calls)
	}
}

func TestHelperCaptchaFromOldConnectionDoesNotPauseReplacement(t *testing.T) {
	pool := newTestPool("shared")
	s := pool.sessions[0]
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		pool.rotate(s)
		return mediaTestResponse(req, http.StatusUnprocessableEntity, nil, []byte(`{}`)), nil
	})}
	s.client = client
	a := &App{pool: pool}
	_, _, err := a.fetchViaProxy(context.Background(), fetchSpec{method: http.MethodGet, url: "https://helper.example/convert", interpret: func(int, []byte) *AppError {
		return igErr(http.StatusBadGateway, errorCodeCaptchaRequired, "verification required")
	}})
	if err == nil || err.Code != errorCodeCaptchaRequired || s.getClient() == client || !s.helperCooldownUntil.IsZero() {
		t.Fatalf("late challenge paused replacement: err=%v cooldown=%v", err, s.helperCooldownUntil)
	}
	s.helperCooldownUntil = time.Now().Add(helperCaptchaCooldown)
	pool.rotate(s)
	if !s.helperCooldownUntil.IsZero() {
		t.Fatal("rotation did not clear the previous IP's helper cooldown")
	}
}

func newTestPool(names ...string) *SessionPool {
	p := &SessionPool{budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 30}
	for _, n := range names {
		p.sessions = append(p.sessions, &Session{name: n})
	}
	return p
}

// A fresh pool whose third session hangs on its first request used to receive
// almost every later pick (28 of 30) until that request timed out.
func TestPickAvoidsHungSession(t *testing.T) {
	p := newTestPool("us-1", "us-2", "us-3")
	counts := map[string]int{}
	for range 30 {
		s, _ := p.pick(context.Background(), false)
		counts[s.name]++
		if s.name != "us-3" {
			p.done(s, 300*time.Millisecond, true)
		}
	}
	if counts["us-3"] > 3 {
		t.Fatalf("hung session received %d of 30 picks: %v", counts["us-3"], counts)
	}
}

func TestPickSpreadsLoadAcrossHealthySessions(t *testing.T) {
	p := newTestPool("us-1", "us-2", "us-3", "us-4")
	counts := map[string]int{}
	for range 400 {
		s, _ := p.pick(context.Background(), false)
		counts[s.name]++
		p.done(s, 300*time.Millisecond, true)
	}
	for _, s := range p.sessions {
		if counts[s.name] < 40 {
			t.Fatalf("equal sessions should share traffic, got %v", counts)
		}
	}
}

func TestPeakRTTAvoidsThenRetriesSlowSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestPool("fast", "slow")
		fast, slow := p.sessions[0], p.sessions[1]
		for _, s := range []*Session{fast, slow} {
			s.pending++
			p.done(s, 300*time.Millisecond, true)
		}
		slow.pending++
		p.done(slow, 10*time.Second, true) // one peak replaces the estimate at once
		picks := func() (n int) {
			for range 50 {
				s, _ := p.pick(context.Background(), false)
				p.done(s, 0, false)
				if s == slow {
					n++
				}
			}
			return n
		}
		if n := picks(); n != 0 {
			t.Fatalf("slow session picked %d of 50 right after a 10s peak", n)
		}
		// The busy session keeps fresh samples while the idle slow one decays,
		// so after a while the slow one is worth retrying.
		time.Sleep(time.Minute)
		fast.pending++
		p.done(fast, 300*time.Millisecond, true)
		if n := picks(); n == 0 {
			t.Fatal("a decayed peak should let the slow session be retried")
		}
	})
}
