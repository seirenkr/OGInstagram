package main

import (
	"context"
	"errors"
	"io"
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
	if p.Username != "instagram" || p.FullName != "Instagram" {
		t.Errorf("author fields: %+v", p)
	}
	if p.Shortcode != "DaD8phTyclR" || !strings.Contains(p.Caption, "time to create") {
		t.Errorf("post fields: %+v", p)
	}
	att := p.Attachments[0]
	if att.Kind != "image" || att.Width != 640 || att.Height != 800 {
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
	v, err := stagedFetch(ctx, stagedSource[string]{fetch: str("a", nil)},
		stagedSource[string]{after: time.Second, fetch: func(context.Context) (string, *AppError) { hedgeCalled = true; return "b", nil }})
	if v != "a" || err != nil || hedgeCalled {
		t.Fatalf("primary win = %q %v hedge=%v", v, err, hedgeCalled)
	}

	started := time.Now()
	v, err = stagedFetch(ctx, stagedSource[string]{fetch: str("", ephemeralErr(502, errorCodeConnection, "x"))},
		stagedSource[string]{after: time.Second, fetch: str("b", nil)})
	if v != "b" || err != nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("failure did not pull the hedge in early: %q %v %s", v, err, time.Since(started))
	}

	// A permanent error outranks a later transient one.
	_, err = stagedFetch(ctx, stagedSource[string]{fetch: str("", igErr(404, errorCodeNotFound, "x"))},
		stagedSource[string]{after: time.Second, fetch: str("", ephemeralErr(502, errorCodeConnection, "x"))})
	if err == nil || err.Code != errorCodeNotFound {
		t.Fatalf("preferred error = %v", err)
	}

	_, err = stagedFetch(ctx, stagedSource[string]{fetch: str("", ephemeralErr(502, errorCodeConnection, "x"))})
	if err == nil || err.Code != errorCodeConnection {
		t.Fatalf("single source error = %v", err)
	}

	v, err = stagedFetch(ctx, stagedSource[string]{fetch: func(context.Context) (string, *AppError) { panic("boom") }},
		stagedSource[string]{after: time.Second, fetch: str("b", nil)})
	if v != "b" || err != nil {
		t.Fatalf("panicking source should fail like any other: %q %v", v, err)
	}
}

// proxyOnce sends spec through a fresh one-session pool and reports whether
// that session was rotated into a cooldown.
func proxyOnce(spec fetchSpec, rt roundTripFunc, timeout time.Duration) (out string, err *AppError, s *Session, rotated bool) {
	client := &http.Client{Transport: rt}
	s = &Session{client: client}
	a := &App{pool: &SessionPool{sessions: []*Session{s}}}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, out, err = a.fetchViaProxy(ctx, spec)
	return out, err, s, s.getClient() != client && s.cooldownUntil.After(time.Now())
}

func TestFetchViaProxyBlamesOnlyUpstreamFailures(t *testing.T) {
	respond := func(status int, body string) roundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			return mediaTestResponse(r, status, nil, []byte(body)), nil
		}
	}
	hang := func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }
	htmlPage := func(r *http.Request) (*http.Response, error) {
		return mediaTestResponse(r, 404, http.Header{"Content-Type": {"text/html"}}, []byte("<html>")), nil
	}
	synctest.Test(t, func(t *testing.T) {
		for _, tt := range []struct {
			name      string
			interpret func(int, []byte) *AppError
			rt        roundTripFunc
			code      string
			rotated   bool
			permanent bool
		}{
			{"429", instagramJSON, respond(429, "{}"), errorCodeRateLimited, true, false},
			{"refused", instagramJSON, func(*http.Request) (*http.Response, error) { return nil, errors.New("refused") }, errorCodeConnection, true, false},
			{"hung", instagramJSON, hang, errorCodeConnection, true, false},
			{"short deadline", instagramJSON, hang, errorCodeConnection, false, false},
			{"ok", instagramJSON, respond(200, "{}"), "", false, false},
			{"login wall", instagramJSON, respond(200, "<!DOCTYPE html><html>"), errorCodeLoginRequired, true, false},
			{"require_login", instagramJSON, respond(200, `{"require_login":true}`), errorCodeLoginRequired, true, false},
			{"caption require_login", instagramJSON, respond(200, `{"caption":"how to fix require_login"}`), "", false, false},
			{"bad json", instagramJSON, respond(200, "{"), errorCodeJSONDecode, true, false},
			{"status fail", instagramJSON, respond(200, `{"status":"fail"}`), errorCodeGraphQL, false, false},
			{"400", instagramJSON, respond(400, `{"status":"fail"}`), errorCodeBadRequest, false, false},
			{"401", instagramJSON, respond(401, `{"message":"Please wait a few minutes"}`), errorCodeUnauthorized, true, false},
			{"404", instagramJSON, respond(404, "{}"), errorCodeNotFound, false, true},
			{"graphql throttle", graphQLJSON, respond(200, `{"data":null,"errors":[{"code":1675004,"message":"x"}]}`), errorCodeRateLimited, true, false},
			{"graphql please wait", graphQLJSON, respond(200, `{"data":null,"errors":[{"code":1,"message":"Please wait a few minutes"}]}`), errorCodeRateLimited, true, false},
			{"graphql 400", graphQLJSON, respond(400, `{"status":"fail"}`), errorCodeGraphQL, false, false},
			{"graphql html 404", graphQLJSON, htmlPage, errorCodeGraphQL, false, false},
		} {
			timeout := 5 * time.Second
			if tt.name == "short deadline" {
				timeout = 2 * time.Second
			}
			_, err, s, rotated := proxyOnce(fetchSpec{method: "GET", url: "https://www.instagram.com/x", expectJSON: true, interpret: tt.interpret}, tt.rt, timeout)
			code := ""
			if err != nil {
				code = err.Code
			}
			if code != tt.code || rotated != tt.rotated || (code != "" && isTransient(code) == tt.permanent) || (tt.code == "" && s.rttAt.IsZero()) || s.pending != 0 {
				t.Errorf("%s: code=%q rotated=%v measured=%v pending=%d, want %q rotated=%v permanent=%v", tt.name, code, rotated, !s.rttAt.IsZero(), s.pending, tt.code, tt.rotated, tt.permanent)
			}
		}
	})
}

func TestOembedHTMLErrorPageClassification(t *testing.T) {
	page := func(status int, contentType, body string) roundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			return mediaTestResponse(r, status, http.Header{"Content-Type": {contentType}}, []byte(body)), nil
		}
	}
	const gated = `{"status":"fail","message":"geoblock_required","gating_type_thrift":3}`
	for _, tt := range []struct {
		name    string
		rt      roundTripFunc
		code    string
		rotated bool
	}{
		{"html 404", page(404, "text/html", "<html>"), errorCodeNotFound, false},
		{"html 429", page(429, "text/html", "<html>"), errorCodeRateLimited, true},
		{"json 429", page(429, "application/json", "{}"), errorCodeRateLimited, true},
		{"json 500", page(500, "application/json", "{}"), errorCodeUpstream, true},
		{"json 400", page(400, "application/json", gated), "", false},
	} {
		out, err, _, rotated := proxyOnce(oembedSpec("x"), tt.rt, 5*time.Second)
		code := ""
		if err != nil {
			code = err.Code
		}
		if code != tt.code || rotated != tt.rotated || (code == "" && out != gated) {
			t.Errorf("%s: code=%q rotated=%v body=%q, want %q rotated=%v", tt.name, code, rotated, out, tt.code, tt.rotated)
		}
	}
}

func TestOembedVerdictGating(t *testing.T) {
	for _, tt := range []struct {
		gating string
		status int
		code   string
	}{
		{`,"gating_type_thrift":2`, 451, errorCodeGeoBlocked},
		{`,"gating_type_thrift":3`, 403, errorCodeRestricted},
		{`,"gating_type_thrift":12`, 403, errorCodeRestricted},
		{"", 403, errorCodeRestricted},
	} {
		body := `{"status":"fail","message":"geoblock_required","title":"Restricted Post","description":"d"` + tt.gating + `}`
		base := *ephemeralErr(http.StatusGatewayTimeout, errorCodeConnection, "deadline")
		base.Final = true
		_, err := oembedVerdict(oembedOutcome{status: 400, body: body, ok: true}, "x", base)
		if err.Status != tt.status || err.Code != tt.code || err.CardCode != tt.code || isTransient(err.Code) ||
			err.Ephemeral || err.Final || err.CardTitle != "Restricted Post" {
			t.Errorf("%q: %+v", tt.gating, err)
		}
	}
}

func TestFetchRequestCountsWireBytes(t *testing.T) {
	wire := compressedResponse(t, "gzip", []byte(`{"data":{}}`))
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mediaTestResponse(r, 200, http.Header{"Content-Encoding": {"gzip"}}, wire), nil
	})}
	before := outboundCounter("wire_test").Load()
	if _, _, err := fetchRequest(context.Background(), client, fetchSpec{operation: "wire_test", method: http.MethodGet, url: "https://example.com/", interpret: statusOnly}); err != nil {
		t.Fatal(err)
	}
	if got := outboundCounter("wire_test").Load() - before; got != int64(len(wire)) {
		t.Fatalf("counted %d bytes, want the %d compressed bytes", got, len(wire))
	}
}

func TestFetchRequestReadsJSONLabeledAsHTML(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		page       bool
	}{
		{"json error", `{"success":false,"response":4}`, false},
		{"html page", "<html><title>x</title></html>", true},
		{"oversized json", `{"x":"` + strings.Repeat("a", pageProbeBytes) + `"}`, true},
	} {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return mediaTestResponse(r, 200, http.Header{"Content-Type": {"text/html; charset=utf-8"}}, []byte(tt.body)), nil
		})}
		_, out, err := fetchRequest(context.Background(), client, fetchSpec{method: http.MethodPost, url: "https://example.com/api", expectJSON: true, interpret: statusOnly})
		if gotPage := err != nil && strings.Contains(err.PublicMessage, "page instead of JSON"); gotPage != tt.page || (!tt.page && (err != nil || out != tt.body)) {
			t.Errorf("%s: out=%q err=%v, want page=%v", tt.name, out, err, tt.page)
		}
	}
}

func TestFetchRequestProbesHTMLWithinLimit(t *testing.T) {
	var read int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		page := strings.NewReader("<html>" + strings.Repeat("x", 1<<20))
		body := readerFunc(func(p []byte) (int, error) { n, err := page.Read(p); read += n; return n, err })
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(body), Request: r}, nil
	})}
	if _, _, err := fetchRequest(context.Background(), client, fetchSpec{method: http.MethodGet, url: "https://example.com/api", expectJSON: true, interpret: statusOnly}); err == nil || read > pageProbeBytes+1 {
		t.Fatalf("err=%v read=%d, want a page error after at most %d bytes", err, read, pageProbeBytes+1)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestLogOutboundSurvivesUnparsableURL(t *testing.T) {
	logOutbound(context.Background(), "op", "direct", http.MethodGet, "https://example.com/p#%zz", time.Now(), 0, 0,
		causedErr(502, errorCodeConnection, "boom", errors.New("boom")), false)
}

func TestLateFailureFromRetiredClientSparesReplacement(t *testing.T) {
	pool := newTestPool("shared")
	s := pool.sessions[0]
	replacement := &http.Client{}
	measured := time.Now()
	s.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		s.mu.Lock() // a concurrent failure already rotated the slot
		s.client, s.rttNs, s.rttAt = replacement, 1, measured
		s.mu.Unlock()
		return mediaTestResponse(req, http.StatusTooManyRequests, nil, []byte(`{}`)), nil
	})}
	a := &App{pool: pool}
	_, _, err := a.fetchViaProxy(context.Background(), fetchSpec{method: http.MethodGet, url: instagramGraphQLOrigin + "/graphql/query", interpret: instagramJSON})
	if err == nil || err.Code != errorCodeRateLimited || s.getClient() != replacement || !s.cooldownUntil.IsZero() ||
		!s.rttAt.Equal(measured) || s.pending != 0 {
		t.Fatalf("late failure touched the replacement: err=%v cooldown=%v rttAt=%v pending=%d", err, s.cooldownUntil, s.rttAt, s.pending)
	}
}

func newTestPool(names ...string) *SessionPool {
	p := &SessionPool{}
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
		s := p.pick(false)
		counts[s.name]++
		if s.name != "us-3" {
			p.done(s, nil, 300*time.Millisecond, true)
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
		s := p.pick(false)
		counts[s.name]++
		p.done(s, nil, 300*time.Millisecond, true)
	}
	for _, s := range p.sessions {
		if counts[s.name] < 40 {
			t.Fatalf("equal sessions should share traffic, got %v", counts)
		}
	}
}

func TestPickKeepsWWWOnFirstSessions(t *testing.T) {
	p := newTestPool("us-1", "us-2", "us-3", "us-4", "us-5")
	for range 50 {
		s := p.pick(true)
		p.done(s, nil, 300*time.Millisecond, true)
		if s.name == "us-4" || s.name == "us-5" {
			t.Fatalf("www request picked %s", s.name)
		}
	}
	for _, s := range p.sessions[:wwwSessions] {
		s.cooldownUntil = time.Now().Add(time.Minute)
	}
	if s := p.pick(true); s == nil || (s.name != "us-4" && s.name != "us-5") {
		t.Fatalf("www did not fall back to the remaining sessions: %v", s)
	}
}

func TestPickHonorsCooldown(t *testing.T) {
	p := newTestPool("us-1")
	s := p.sessions[0]
	s.cooldownUntil = time.Now().Add(time.Minute)
	if got := p.pick(false); got != nil {
		t.Fatalf("pick during cooldown = %v, want none", got)
	}
	s.cooldownUntil = time.Now().Add(-time.Second)
	if got := p.pick(false); got != s {
		t.Fatalf("pick after cooldown = %v, want the session", got)
	}
}

func TestPeakRTTAvoidsThenRetriesSlowSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestPool("fast", "slow")
		fast, slow := p.sessions[0], p.sessions[1]
		for _, s := range []*Session{fast, slow} {
			s.pending++
			p.done(s, nil, 300*time.Millisecond, true)
		}
		slow.pending++
		p.done(slow, nil, 10*time.Second, true) // one peak replaces the estimate at once
		picks := func() (n int) {
			for range 50 {
				s := p.pick(false)
				p.done(s, nil, 0, false)
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
		p.done(fast, nil, 300*time.Millisecond, true)
		if n := picks(); n == 0 {
			t.Fatal("a decayed peak should let the slow session be retried")
		}
	})
}
