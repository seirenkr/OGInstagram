package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	store := newTestStore(t)
	cfg := Config{BaseURL: "https://oginstagram.com", AllowedHosts: []string{"oginstagram.com", "g.oginstagram.com", "d.oginstagram.com"}, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.30.0.3/32")}, Store: store, TurnstileSecretKey: "test", AdminPurgeToken: "admin-test"}
	return newGateway(cfg, newApp(cfg, newSessionPool(cfg), mustOffloadSigner(testOffloadSigningKeys)), nil)
}

// serveApp is Gateway.render without the gateway's admission checks and timeout.
func serveApp(t *testing.T, a *App, r *http.Request) resp {
	t.Helper()
	route, ok := resolveGatewayRoute(r)
	if !ok {
		t.Fatalf("%s %s not routed", r.Host, r.URL)
	}
	return route.handle(a, r)
}

func publicRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, "https://oginstagram.com"+path, nil)
	r.RemoteAddr = "172.30.0.3:3000"
	return r
}

func TestGatewayRejectsUntrustedPeersAndHosts(t *testing.T) {
	g := testGateway(t)
	for _, tt := range []struct {
		dev        bool
		peer, host string
		status     int
	}{
		{false, "203.0.113.9:1000", "oginstagram.com", 403},
		{false, "172.30.0.3:1000", "attacker.example", 421},
		{false, "172.30.0.30:1000", "oginstagram.com", 403},
		{false, "127.0.0.1:1", "oginstagram.com", 403},
		{true, "203.0.113.9:1", "localhost", 403},
		{true, "127.0.0.1:1", "localhost", 200},
		{false, "172.30.0.3:1", "oginstagram.com:443", 421},
		{false, "172.30.0.3:1", "OGINSTAGRAM.COM", 421},
	} {
		r := publicRequest("GET", "/api/status")
		r.RemoteAddr = tt.peer
		r.Host = tt.host
		g.cfg.Development = tt.dev
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("dev=%v peer=%s host=%s got %d want%d", tt.dev, tt.peer, tt.host, w.Code, tt.status)
		}
	}
}

func TestGatewayWebFingerUsesValidatedHost(t *testing.T) {
	g := testGateway(t)
	r := publicRequest("GET", "/.well-known/webfinger?resource=acct:alice@oginstagram.com")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://oginstagram.com/users/alice") {
		t.Fatalf("unsafe/missing WebFinger: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cloudflare-CDN-Cache-Control") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("dynamic response could bypass gateway")
	}
	g.cfg.AssetsDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(g.cfg.AssetsDir, "favicon-64.png"), []byte("png"), 0600); err != nil {
		t.Fatal(err)
	}
	asset := httptest.NewRecorder()
	g.ServeHTTP(asset, publicRequest("GET", "/favicon-64.png"))
	for _, h := range []http.Header{w.Header(), asset.Header()} {
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("security headers missing: %v", h)
		}
	}
	if asset.Code != 200 || asset.Body.String() != "png" {
		t.Fatalf("asset = %d %q", asset.Code, asset.Body.String())
	}
}

func TestGatewayBotHumanGalleryDirectRoutes(t *testing.T) {
	g := testGateway(t)
	exp := time.Now().Add(time.Hour)
	var media []Attachment
	for _, name := range []string{"1", "2", "3"} {
		media = append(media, Attachment{Kind: "image", URL: "https://scontent.cdninstagram.com/" + name + ".jpg"})
	}
	cdn := "https://scontent.cdninstagram.com/"
	media[1].Thumbnail = cdn + "t2.jpg"
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{value: Post{Shortcode: "ABC123", Username: "alice", Caption: "caption", ProfilePic: cdn + "pa.jpg", Attachments: media}, expiresAt: exp})
	g.app.profiles.storeLocal("alice", &cacheEntry[Profile]{value: Profile{Username: "alice", Biography: "bio", ProfilePic: cdn + "a.jpg", RecentMedia: []ProfileMedia{{Thumbnail: cdn + "r1.jpg"}}}, expiresAt: exp})
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{value: Story{ID: "123", Username: "alice", Caption: "story caption", ProfilePic: cdn + "sa.jpg", Media: Attachment{Kind: "image", URL: cdn + "s.jpg"}}, expiresAt: exp})
	// Direct hosts and offload paths redirect to the media; gallery hosts (never a
	// client query) blank the description. Offload signatures are checked in ServeHTTP.
	for _, tt := range []struct {
		host, path, redirect, category, location string
		gallery                                  bool
	}{
		{"oginstagram.com", "/p/ABC123", "https://www.instagram.com/p/ABC123/", "posts", "", false},
		{"g.oginstagram.com", "/alice/p/ABC123/2?__gallery=bad", "https://www.instagram.com/p/ABC123/?img_index=2", "posts", "", true},
		{"d.oginstagram.com", "/reel/ABC123?img_index=3", "", "posts", "https://scontent.cdninstagram.com/3.jpg", false},
		{"d.oginstagram.com", "/stories/Alice/123", "", "stories", "https://scontent.cdninstagram.com/s.jpg", false},
		{"g.oginstagram.com", "/stories/alice/123", storyOriginURL("alice", "123"), "stories", "", true},
		{"d.test.example", "/p/ABC123", "", "posts", "https://scontent.cdninstagram.com/1.jpg", false},
		{"www.g.test.example", "/p/ABC123", "https://www.instagram.com/p/ABC123/", "posts", "", true},
		{"oginstagram.com", "/alice?__gallery=1", "https://www.instagram.com/alice/", "profile", "", false},
		{"g.oginstagram.com", "/alice", "https://www.instagram.com/alice/", "profile", "", true},
		{"oginstagram.com", "/offload/ABC123/2", "", "", cdn + "2.jpg", false},
		{"oginstagram.com", "/offload/ABC123/2?thumbnail=1", "", "", cdn + "t2.jpg", false},
		{"oginstagram.com", "/offload/ABC123/avatar", "", "", cdn + "pa.jpg", false},
		{"oginstagram.com", "/offload/@Alice/1", "", "", cdn + "r1.jpg", false},
		{"oginstagram.com", "/offload/@alice", "", "", cdn + "a.jpg", false},
		{"oginstagram.com", "/offload/story/alice/123/avatar", "", "", cdn + "sa.jpg", false},
	} {
		r := publicRequest("GET", tt.path)
		r.Host = tt.host
		got, ok := resolveGatewayRoute(r)
		if !ok || got.humanRedirect != tt.redirect || got.category != tt.category {
			t.Fatalf("route %s %s = %+v,%v", tt.host, tt.path, got, ok)
		}
		res := got.handle(g.app, r)
		if tt.location != "" {
			if res.status != 302 || res.headers["Location"] != tt.location {
				t.Fatalf("direct %s %s = %d %v", tt.host, tt.path, res.status, res.headers)
			}
		} else if res.status != 200 || strings.Contains(string(res.body), `name="description" content=""`) != tt.gallery {
			t.Fatalf("render %s %s = %d %.400s", tt.host, tt.path, res.status, res.body)
		}
	}
	// In range for the URL, but past what the profile actually has.
	if res := serveApp(t, g.app, publicRequest("GET", "/offload/@alice/2")); res.status != 404 {
		t.Fatalf("missing profile media = %d", res.status)
	}
	// Only browser document navigations are humans; the User-Agent is irrelevant.
	r := publicRequest("GET", "/p/ABC123/2/")
	r.Header.Set("User-Agent", "Mozilla/5.0 [LinkedInApp]")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 307 || w.Header().Get("Location") != "https://www.instagram.com/p/ABC123/?img_index=2" {
		t.Fatalf("human redirect: %+v", w.Result())
	}
}

func TestGatewayBotUnfurls(t *testing.T) {
	g := testGateway(t)
	exp := time.Now().Add(time.Hour)
	video := Attachment{Kind: "video", URL: "https://scontent.cdninstagram.com/v.mp4", Thumbnail: "https://scontent.cdninstagram.com/t.jpg", Width: 720, Height: 1280}
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{value: Story{ID: "123", Username: "alice", Media: video}, expiresAt: exp})
	g.app.profiles.storeLocal("alice", &cacheEntry[Profile]{value: Profile{Username: "alice", ProfilePic: "https://scontent.cdninstagram.com/a.jpg"}, expiresAt: exp})
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{value: Post{Shortcode: "ABC123", Username: "alice", Attachments: []Attachment{video}}, expiresAt: exp})
	g.app.posts.storeLocal("GONE12", &cacheEntry[Post]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: exp})
	g.app.profiles.storeLocal("bob", &cacheEntry[Profile]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: exp})
	story := storyStatusSnowcode("alice", "123", false)
	for _, tt := range []struct {
		method, path, want string
		status             int
	}{
		{"GET", "/stories/alice/123", `og:video:type`, 200},
		{"GET", "/alice", `og:type" content="profile`, 200},
		{"GET", "/p/ABC123", `og:video:type`, 200},
		{"GET", "/api/v1/statuses/" + story, `media_attachments`, 200},
		{"GET", "/api/v1/statuses/" + profileSnowcode("alice"), `media_attachments`, 200},
		{"GET", "/api/v1/statuses/" + profileSnowcode("bob"), `"error"`, 404},
		{"GET", "/users/alice/statuses/" + story, `Note`, 200},
		{"GET", "/users/alice/statuses/" + profileSnowcode("alice"), `Note`, 200},
		{"GET", "/users/alice", `Person`, 200},
		{"GET", "/users/alice/outbox", `OrderedCollection`, 200},
		{"GET", "/p/GONE12", `Post unavailable`, 200},
		{"GET", "/bob", `Account unavailable`, 200},
		{"POST", "/users/alice/inbox", ``, 405},
	} {
		r := publicRequest(tt.method, tt.path)
		r.Header.Set("User-Agent", "Discordbot/2.0")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.want) {
			t.Errorf("%s %s = %d %.300s", tt.method, tt.path, w.Code, w.Body.String())
		}
	}
}

func TestGatewayOffloadAuthBeforeWarmModel(t *testing.T) {
	g := testGateway(t)
	post := Post{Shortcode: "ABC123", Username: "alice", Attachments: []Attachment{{URL: "https://scontent.cdninstagram.com/photo.jpg", Kind: "image"}}}
	data, _ := json.Marshal(cachePayload[Post]{Value: post})
	if err := g.cfg.Store.putModel(context.Background(), "post", "ABC123", data, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	signed := g.app.offloadSigner.url("https://oginstagram.com", "/offload/ABC123/1", false)
	valid, _ := url.Parse(signed)
	for _, tt := range []struct {
		query  string
		status int
	}{
		{valid.RawQuery, 302}, {"", 404}, {valid.RawQuery + "&sig=duplicate", 404}, {strings.Replace(valid.RawQuery, "v=2", "v=3", 1), 404},
	} {
		r := publicRequest("GET", "/offload/ABC123/1?"+tt.query)
		r.Header.Set("User-Agent", "Discordbot")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("offload got%d want%d body%s", w.Code, tt.status, w.Body.String())
		}
	}
}

func TestValidPreviewPaths(t *testing.T) {
	r := publicRequest("POST", "/api/embed")
	for _, path := range []string{"/p/ABC123", "/alice/reel/ABC123/2", "/p/ABC123?img_index=1", "/alice", "/stories/alice/123"} {
		if _, _, ok := previewTarget(r, path); !ok {
			t.Errorf("valid path rejected: %s", path)
		}
	}
	for _, path := range []string{"//attacker.example/p/A", "https://attacker.example/p/A", "/offload/A", "/offload/ABC123/1", "/users/p/inbox", "/users/p/statuses/123",
		"/.well-known/webfinger", "/p/A?img_index=1&img_index=2", "/p/A?img_index=0", "/p/A#fragment", "/p/A?__gallery=1", "/alice?x=1", "/p/A\\bad", "/stories/alice/nope"} {
		if _, _, ok := previewTarget(r, path); ok {
			t.Errorf("unsafe path accepted: %s", path)
		}
	}
}

func TestPreviewRejectsBadOriginAndDuplicateClearance(t *testing.T) {
	g := testGateway(t)
	for _, tt := range []struct{ origin, cookie string }{{"https://attacker.example", "cf_clearance=a"}, {"https://oginstagram.com", ""}, {"https://oginstagram.com", "cf_clearance=a; cf_clearance=b"}} {
		r := publicRequest("POST", "/api/embed")
		r.Body = io.NopCloser(strings.NewReader(`{"path":"/p/ABC123"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("Cookie", tt.cookie)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("preview got%d for %+v", w.Code, tt)
		}
	}
	now := time.Now()
	for i := 0; i < 8; i++ {
		if !g.allowClearance("one", now) {
			t.Fatal("limit early")
		}
	}
	if g.allowClearance("one", now) || !g.allowClearance("one", now.Add(time.Minute)) {
		t.Fatal("rate window wrong")
	}
}

func TestGatewayAdmissionLimit(t *testing.T) {
	g := testGateway(t)
	for i := 0; i < maxGatewayInFlight; i++ {
		g.slots <- struct{}{}
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, publicRequest("GET", "/api/status"))
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("overload not bounded: %d", w.Code)
	}
}

func TestGatewayHealthIsLocalOnly(t *testing.T) {
	g := testGateway(t)
	for _, tt := range []struct {
		peer   string
		status int
	}{{"127.0.0.1:2000", 200}, {"172.30.0.3:2000", 404}} {
		r := publicRequest("GET", "/readyz")
		r.RemoteAddr = tt.peer
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("readiness got%d want%d", w.Code, tt.status)
		}
	}
}

func TestGatewayRejectsEncodedProtectedEndpoints(t *testing.T) {
	g := testGateway(t)
	for _, path := range []string{"/api%2Fembed", "/%61pi/embed", "/api/%65mbed", "/api%2Fadmin/purge", "/api/%73tatus"} {
		r := publicRequest("POST", path)
		r.Header.Set("Origin", "https://oginstagram.com")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Cookie", "cf_clearance=fabricated")
		r.Header.Set("Authorization", "Bearer admin-test")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("encoded protected endpoint %s got %d", path, w.Code)
		}
	}
}

func TestTurnstileRequiresSuccessActionAndHostname(t *testing.T) {
	g := testGateway(t)
	for _, tt := range []struct {
		response string
		want     bool
	}{
		{`{"success":true,"action":"turnstile-spin-v1","hostname":"oginstagram.com"}`, true},
		{`{"success":false,"action":"turnstile-spin-v1","hostname":"oginstagram.com"}`, false},
		{`{"success":true,"action":"other","hostname":"oginstagram.com"}`, false},
		{`{"success":true,"action":"turnstile-spin-v1","hostname":"attacker.example"}`, false},
		{`{"success":true}`, false},
	} {
		g.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://challenges.cloudflare.com/turnstile/v0/siteverify" {
				t.Fatal("unexpected verification URL")
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("secret") != "test" || r.Form.Get("response") != "token" {
				t.Fatal("missing verification fields")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tt.response))}, nil
		})}
		if got := g.verifyTurnstile(publicRequest("POST", "/api/embed"), "https://oginstagram.com", "token"); got != tt.want {
			t.Fatalf("verification %s = %v", tt.response, got)
		}
	}
}

func TestPreviewExtendsWriteDeadlineForSiteverify(t *testing.T) {
	g := testGateway(t)
	post := Post{Shortcode: "ABC123", Username: "alice", Attachments: []Attachment{{URL: "https://scontent.cdninstagram.com/photo.jpg", Kind: "image"}}}
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{value: post, expiresAt: time.Now().Add(time.Hour)})
	g.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		time.Sleep(100 * time.Millisecond)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"action":"turnstile-spin-v1","hostname":"oginstagram.com"}`))}, nil
	})}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "172.30.0.3:1000"
		g.ServeHTTP(w, r)
	}))
	srv.Config.WriteTimeout = 25 * time.Millisecond
	srv.Start()
	defer srv.Close()
	r, _ := http.NewRequest("POST", srv.URL+"/api/embed", strings.NewReader(`{"path":"/p/ABC123","token":"token"}`))
	r.Host = "oginstagram.com"
	r.Header.Set("Origin", "https://oginstagram.com")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Cookie", "cf_clearance=test")
	r.Header.Set("Content-Type", "application/json")
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), "og:image") {
		t.Fatalf("preview failed: %d %v %s", response.StatusCode, err, body)
	}
}

func TestAdminPurgeAuthenticatesAndPreservesBudget(t *testing.T) {
	g := testGateway(t)
	ctx := context.Background()
	if _, _, err := g.cfg.Store.takeBudget(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	remaining := remainingDailyBudget(t, g.cfg.Store)
	if err := g.cfg.Store.putModel(ctx, "post", "ABC123", []byte(`{}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{expiresAt: time.Now().Add(time.Hour)})
	previewCache.put("purge-test", previewMedia{body: []byte("cached"), expires: time.Now().Add(time.Hour)})
	for _, tt := range []struct {
		token  string
		status int
	}{{"bad", 403}, {"admin-test", 200}} {
		r := publicRequest("POST", "/api/admin/purge")
		r.Header.Set("Authorization", "Bearer "+tt.token)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("purge got %d want %d", w.Code, tt.status)
		}
		if tt.status == 403 {
			if _, ok := g.app.posts.localGet("ABC123"); !ok {
				t.Fatal("unauthorized purge changed cache")
			}
		}
	}
	if _, ok := g.app.posts.localGet("ABC123"); ok {
		t.Fatal("L1 retained purged model")
	}
	if _, _, err := g.cfg.Store.getModel(ctx, "post", "ABC123"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("L2 retained purged model: %v", err)
	}
	if _, ok := previewCache.get("purge-test"); ok {
		t.Fatal("transformed media retained")
	}
	if remainingDailyBudget(t, g.cfg.Store) != remaining {
		t.Fatal("cache purge reset proxy budget")
	}
}
