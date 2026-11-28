package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestParseShareTarget(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		want     shareTarget
		wantCode string
	}{
		{"https://www.instagram.com/reel/DgNN8_uKwFg/?evlk=MTR4enVsaGF4MnluZQ%3D%3D", "/reel/DgNN8_uKwFg", ""},
		{"https://www.instagram.com/p/DaD8phTyclR/?img_index=2&igsh=abc", "/p/DaD8phTyclR?img_index=2", ""},
		{"https://www.instagram.com/alice/reels/DaD8phTyclR/3", "/reel/DaD8phTyclR?img_index=3", ""},
		{"https://www.instagram.com/stories/alice.user/4011387627600768522/?igsh=xyz", "/stories/alice.user/4011387627600768522", ""},
		{"https://m.instagram.com/alice.user/?igsh=xyz", "/alice.user", ""},
		{"https://www.instagram.com/accounts/login/?next=%2Freel%2Fabc%2F", "", errorCodeLoginRequired},
		{"https://www.instagram.com/accounts/onetap/?next=%2Freel%2Fabc%2F", "", errorCodeLoginRequired},
		{"https://m.instagram.com/challenge/123/", "", errorCodeLoginRequired},
		{"https://threads.com/@alice/post/123", "", errorCodeNotFound},
		{"http://www.instagram.com/reel/DgNN8_uKwFg/", "", errorCodeNotFound},
		{"https://www.instagram.com/stories/highlights/1794567890/", "", errorCodeNotFound},
		{"https://www.instagram.com/reels/audio/1234567890/", "", errorCodeNotFound},
		{"https://www.instagram.com/channels/", "", errorCodeNotFound},
		{"https://www.instagram.com/share/BAblI6LDIl/", "", errorCodeNotFound},
		{"https://www.instagram.com/", "", errorCodeNotFound},
		{"https://www.instagram.com/p/________________________/", "", errorCodeNotFound},
	} {
		got, err := parseShareTarget(tc.raw)
		if tc.wantCode != "" {
			if err == nil || err.Code != tc.wantCode {
				t.Errorf("parseShareTarget(%q) = %q, %v; want error %s", tc.raw, got, err, tc.wantCode)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseShareTarget(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}

	for _, bad := range []shareTarget{"", "//evil.example/p/DaD8phTyclR", "/p/DaD8phTyclR/", "/p/DaD8phTyclR?foo=1", "/explore"} {
		if _, ok := cloneAndValidateCacheValue("BAblI6LDIl", bad); ok {
			t.Errorf("cache boundary accepted invalid share target %q", bad)
		}
	}
}

func TestShareRedirectHopsAndLoopCap(t *testing.T) {
	var paths []string
	body := &unreadResponseBody{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/share/BAblI6LDIl/":
			return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {"https://m.instagram.com/share/reel/BAblI6LDIl/"}}, nil), nil
		case "/share/reel/BAblI6LDIl/":
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": {"https://www.instagram.com/reel/DgNN8_uKwFg/?evlk=abc"}},
				Body:       body,
				Request:    r,
			}, nil
		default:
			t.Errorf("unexpected fetch to destination %s", r.URL)
			return mediaTestResponse(r, http.StatusOK, nil, nil), nil
		}
	})}
	a := newApp(Config{}, &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}, offloadSigner{})
	got, err := a.getShareTarget(context.Background(), "BAblI6LDIl", nil)
	if err != nil || got != "/reel/DgNN8_uKwFg" || len(paths) != 2 || paths[0] != "HEAD /share/BAblI6LDIl/" || body.reads != 0 || !body.closed {
		t.Fatalf("getShareTarget = %q, %v; paths=%v reads=%d closed=%t", got, err, paths, body.reads, body.closed)
	}

	var loopCalls int
	loopClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		loopCalls++
		return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {"/share/loop" + strconv.Itoa(loopCalls) + "/"}}, nil), nil
	})}
	a = newApp(Config{}, &SessionPool{sessions: []*Session{{name: "us-1", client: loopClient}}}, offloadSigner{})
	if _, err := a.getShareTarget(context.Background(), "loop0", nil); err == nil || err.Code != errorCodeNotFound || loopCalls != 3 {
		t.Fatalf("share redirect loop: err=%v calls=%d; want not_found after 3 requests", err, loopCalls)
	}
}

func TestShareViaProxyFallbackPolicy(t *testing.T) {
	makePool := func(fn func(*http.Request) (*http.Response, error), prefix string) *SessionPool {
		client := &http.Client{Transport: roundTripFunc(fn)}
		p := &SessionPool{}
		if prefix == "v6p-" {
			p.addrs = newEgressAddrs(egressPoolSize, nil)
			p.newClient = func(*url.URL) *http.Client { return client }
		}
		for i := range 2 {
			addr := -1
			if p.addrs != nil {
				_, addr, _ = p.nextProxy(-1)
			}
			p.sessions = append(p.sessions, &Session{name: prefix + strconv.Itoa(i+1), addr: addr, client: client})
		}
		return p
	}
	for _, tc := range []struct {
		name         string
		v6Status     int
		v6Loc        string
		dcStatus     int
		want         shareTarget
		wantCode     string
		wantDC       int32
		wantV6Strike bool
	}{
		{"v6 200 soft block recovers on DC", http.StatusOK, "", http.StatusFound, "/reel/DgNN8_uKwFg", "", 1, true},
		{"v6 200 and DC fails stays transient", http.StatusOK, "", http.StatusBadGateway, "", errorCodeUpstream, 1, false},
		{"missing token 200 on both", http.StatusOK, "", http.StatusOK, "", errorCodeNotFound, 1, false},
		{"explicit 404 skips DC", http.StatusNotFound, "", http.StatusFound, "", errorCodeNotFound, 0, false},
		{"unsupported target", http.StatusFound, "https://www.instagram.com/stories/highlights/123/", http.StatusFound, "", errorCodeNotFound, 0, false},
		{"external redirect skips DC", http.StatusFound, "https://threads.com/@alice", http.StatusFound, "", errorCodeNotFound, 0, false},
		{"login wall", http.StatusFound, "https://www.instagram.com/accounts/login/?next=%2Fshare%2Fx%2F", http.StatusFound, "/reel/DgNN8_uKwFg", "", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v6Calls, dcCalls atomic.Int32
			a := newApp(Config{EgressPercent: 100}, makePool(func(r *http.Request) (*http.Response, error) {
				dcCalls.Add(1)
				h := http.Header{}
				if tc.dcStatus == http.StatusFound {
					h.Set("Location", "https://www.instagram.com/reel/DgNN8_uKwFg/")
				}
				return mediaTestResponse(r, tc.dcStatus, h, nil), nil
			}, "us-"), offloadSigner{})
			a.v6Pages = makePool(func(r *http.Request) (*http.Response, error) {
				v6Calls.Add(1)
				h := http.Header{}
				if tc.v6Loc != "" {
					h.Set("Location", tc.v6Loc)
				}
				return mediaTestResponse(r, tc.v6Status, h, nil), nil
			}, "v6p-")
			got, err := a.getShareTarget(context.Background(), "BAblI6LDIl", nil)
			if g := a.v6Gate("share"); (g.failures > 0) != tc.wantV6Strike {
				t.Fatalf("v6Gate failures = %d, want strike=%v", g.failures, tc.wantV6Strike)
			}
			if tc.wantCode != "" {
				if err == nil || err.Code != tc.wantCode || v6Calls.Load() != 1 || dcCalls.Load() != tc.wantDC {
					t.Fatalf("err=%v v6=%d dc=%d; want %s dc=%d", err, v6Calls.Load(), dcCalls.Load(), tc.wantCode, tc.wantDC)
				}
				if tc.v6Status == http.StatusOK && tc.dcStatus == http.StatusOK {
					if e, ok := a.shares.localGet("BAblI6LDIl"); !ok || time.Until(e.expiresAt) > time.Duration(transientErrorCacheSeconds+1)*time.Second {
						t.Fatalf("ambiguous 200 share miss cached too long: ok=%v ttl=%v", ok, time.Until(e.expiresAt))
					}
				}
				return
			}
			if err != nil || got != tc.want || v6Calls.Load() != 1 || dcCalls.Load() != tc.wantDC {
				t.Fatalf("got=%q err=%v v6=%d dc=%d; want %q dc=%d", got, err, v6Calls.Load(), dcCalls.Load(), tc.want, tc.wantDC)
			}
		})
	}
}

func TestShareTargetPersistsAndSharesCacheAcrossAliases(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newTestStore(t)
		var calls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return mediaTestResponse(r, http.StatusFound, http.Header{"Location": {"https://www.instagram.com/reel/DgNN8_uKwFg/?evlk=abc"}}, nil), nil
		})}
		cfg := Config{Store: store}
		a := newApp(cfg, &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}, mustOffloadSigner(testOffloadSigningKeys))
		a.posts.storeLocal("DgNN8_uKwFg", &cacheEntry[Post]{
			value:     Post{Shortcode: "DgNN8_uKwFg", Username: "alice", Caption: "hello", Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/1.jpg"}}},
			expiresAt: time.Now().Add(time.Hour),
		})

		first := serveApp(t, a, publicRequest("GET", "/share/BAblI6LDIl"))
		second := serveApp(t, a, publicRequest("GET", "/share/reel/BAblI6LDIl"))
		third := serveApp(t, a, publicRequest("GET", "/share/p/BAblI6LDIl/"))
		if first.cacheHit || !second.cacheHit || !third.cacheHit || calls.Load() != 1 {
			t.Fatalf("cache hits = (%v,%v,%v) calls=%d; want (false,true,true) after 1 lookup", first.cacheHit, second.cacheHit, third.cacheHit, calls.Load())
		}
		synctest.Wait()

		a2 := newApp(cfg, &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}, mustOffloadSigner(testOffloadSigningKeys))
		meta := &fetchMeta{}
		if got, err := a2.getShareTarget(context.Background(), "BAblI6LDIl", meta); err != nil || got != "/reel/DgNN8_uKwFg" || meta.fetched || calls.Load() != 1 {
			t.Fatalf("persisted lookup = %q, %v, fetched=%v, calls=%d", got, err, meta.fetched, calls.Load())
		}
	})
}

func TestGatewayShareRoutes(t *testing.T) {
	g := testGateway(t)
	now := time.Now()
	exp := now.Add(time.Hour)
	cdn := "https://scontent.cdninstagram.com/"
	oe := "?oe=" + strconv.FormatInt(now.Add(3*time.Hour).Unix(), 16)

	g.app.posts.storeLocal("DgNN8_uKwFg", &cacheEntry[Post]{
		value: Post{
			Shortcode: "DgNN8_uKwFg", Username: "alice", Caption: "reel caption",
			Attachments: []Attachment{
				{Kind: "image", URL: cdn + "1.jpg" + oe, Thumbnail: cdn + "t1.jpg" + oe},
				{Kind: "image", URL: cdn + "2.jpg" + oe, Thumbnail: cdn + "t2.jpg" + oe},
				{Kind: "image", URL: cdn + "3.jpg" + oe, Thumbnail: cdn + "t3.jpg" + oe},
			},
		},
		expiresAt: exp,
	})
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{
		value: Story{ID: "123", Username: "alice", Caption: "story caption", Media: Attachment{Kind: "image", URL: cdn + "s.jpg" + oe}}, expiresAt: exp,
	})
	g.app.profiles.storeLocal("alice", &cacheEntry[Profile]{
		value: Profile{Username: "alice", Biography: "bio", ProfilePic: cdn + "a.jpg" + oe}, expiresAt: exp,
	})
	g.app.shares.storeLocal("REEL1", &cacheEntry[shareTarget]{value: "/reel/DgNN8_uKwFg?img_index=2", expiresAt: exp})
	g.app.shares.storeLocal("STORY1", &cacheEntry[shareTarget]{value: "/stories/alice/123", expiresAt: exp})
	g.app.shares.storeLocal("PROF1", &cacheEntry[shareTarget]{value: "/alice", expiresAt: exp})
	g.app.shares.storeLocal("GONE1", &cacheEntry[shareTarget]{err: igErr(http.StatusNotFound, errorCodeNotFound, "share link not found"), expiresAt: exp})

	nav := publicRequest("GET", "/share/p/UNRESOLVED/2/")
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	nav.Header.Set("Sec-Fetch-Dest", "document")
	nw := httptest.NewRecorder()
	g.ServeHTTP(nw, nav)
	if nw.Code != http.StatusTemporaryRedirect || nw.Header().Get("Location") != "https://www.instagram.com/share/p/UNRESOLVED/?img_index=2" {
		t.Fatalf("human share redirect = %d %q", nw.Code, nw.Header().Get("Location"))
	}

	for _, tt := range []struct {
		host, path, wantBody, location string
		status, edgeMaxAge             int
		revalidate                     bool
	}{
		{"oginstagram.com", "/share/REEL1", `https://www.instagram.com/reel/DgNN8_uKwFg/?img_index=2`, "", 200, cardEdgeTTL, false},
		{"g.oginstagram.com", "/share/reel/REEL1/3", `name="description" content=""`, "", 200, cardEdgeTTL, false},
		{"d.oginstagram.com", "/share/REEL1", "", cdn + "2.jpg" + oe, 302, 9000, true},
		{"d.oginstagram.com", "/share/REEL1?img_index=3", "", cdn + "3.jpg" + oe, 302, 9000, true},
		{"d.oginstagram.com", "/share/REEL1?thumbnail=1", "", cdn + "t2.jpg" + oe, 302, 9000, true},
		{"oginstagram.com", "/share/STORY1", `story caption`, "", 200, cardEdgeTTL, false},
		{"d.oginstagram.com", "/share/STORY1", "", cdn + "s.jpg" + oe, 302, 9000, true},
		{"oginstagram.com", "/share/PROF1", `og:type" content="profile`, "", 200, cardEdgeTTL, false},
		{"oginstagram.com", "/share/GONE1", `Link unavailable`, "", 200, 0, false},
		{"d.oginstagram.com", "/share/GONE1", `share link not found`, "", 404, 0, false},
	} {
		r := publicRequest("GET", tt.path)
		r.Host = tt.host
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		edge := w.Header().Get("Cloudflare-CDN-Cache-Control")
		m := edgeCacheControl.FindStringSubmatch(edge)
		age := 0
		if m != nil {
			age, _ = strconv.Atoi(m[1])
		}
		if w.Code != tt.status ||
			(tt.location != "" && w.Header().Get("Location") != tt.location) ||
			(tt.wantBody != "" && !strings.Contains(w.Body.String(), tt.wantBody)) ||
			(tt.edgeMaxAge == 0 && edge != "") ||
			(tt.edgeMaxAge > 0 && (m == nil || age < tt.edgeMaxAge-2 || age > tt.edgeMaxAge || (m[2] != "") != tt.revalidate)) {
			t.Errorf("%s%s = %d loc=%q edge=%q body=%.240s", tt.host, tt.path, w.Code, w.Header().Get("Location"), edge, w.Body.String())
		}
	}

	ap := publicRequest("GET", "/share/REEL1")
	ap.Header.Set("Accept", asMediaType)
	apw := httptest.NewRecorder()
	g.ServeHTTP(apw, ap)
	if apw.Code != 200 || apw.Header().Get("Content-Type") != asMediaType || apw.Header().Get("Cloudflare-CDN-Cache-Control") != "" ||
		!strings.Contains(apw.Body.String(), `"https://oginstagram.com/reel/DgNN8_uKwFg/2"`) {
		t.Fatalf("activity share = %d %v %s", apw.Code, apw.Header(), apw.Body.String())
	}

	for _, tt := range []struct {
		body   string
		status int
		want   string
	}{
		{`{"path":"/share/REEL1"}`, 200, "reel caption"},
		{`{"path":"/share/GONE1"}`, 404, "preview unavailable"},
	} {
		r := publicRequest("POST", "/api/embed")
		r.Body = io.NopCloser(strings.NewReader(tt.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://oginstagram.com")
		r.Header.Set("Cookie", "cf_clearance=test")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.want) {
			t.Errorf("preview %s = %d %s, want %d containing %q", tt.body, w.Code, w.Body.String(), tt.status, tt.want)
		}
	}

	if route, ok := resolveGatewayRoute(publicRequest("GET", "/share/GONE1")); !ok || route.category != "share" {
		t.Fatalf("share route = %+v, %v; want share category", route, ok)
	}
	if _, ok := resolveGatewayRoute(publicRequest("GET", "/explore/inbox")); ok {
		t.Fatal("/explore/inbox routed as an ActivityPub collection")
	}
	if w := httptest.NewRecorder(); func() int {
		g.ServeHTTP(w, publicRequest("GET", "/.well-known/webfinger?resource=acct:explore@oginstagram.com"))
		return w.Code
	}() != http.StatusNotFound {
		t.Fatalf("reserved WebFinger status = %d, want 404", w.Code)
	}
}
