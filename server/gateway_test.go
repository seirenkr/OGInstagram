package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testGateway(t *testing.T) *Gateway {
	t.Helper()
	store := newTestStore(t)
	cfg := Config{BaseURL: "https://oginstagram.com", AllowedHosts: []string{"oginstagram.com", "g.oginstagram.com", "d.oginstagram.com"}, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.30.0.3/32")}, Store: store, TurnstileSecretKey: "test"}
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected outbound request: %s", r.URL)
		return nil, errors.New("unexpected outbound request")
	}))
	return newGateway(cfg, newApp(cfg, newSessionPool(cfg), mustOffloadSigner(testOffloadSigningKeys)), nil)
}

// serveApp is Gateway.render without the gateway's admission checks and timeout.
func serveApp(t *testing.T, a *App, r *http.Request) resp {
	t.Helper()
	r = withOrigin(r)
	route, ok := resolveGatewayRoute(r)
	if !ok {
		t.Fatalf("%s %s not routed", r.Host, r.URL)
	}
	return route.handle(a, r)
}

// withOrigin stores the origin Gateway.ServeHTTP would validate.
func withOrigin(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), publicOriginKey{}, "https://"+r.Host))
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
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"https://oginstagram.com/alice"`) {
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

func TestGatewayPassesItsOriginToHandlers(t *testing.T) {
	g := testGateway(t)
	g.cfg.AllowedHosts = append(g.cfg.AllowedHosts, "localhost")
	for _, tt := range []struct {
		dev              bool
		peer, host, want string
	}{
		{true, "127.0.0.1:1", "localhost:5173", "http://localhost:5173"},
		{false, "172.30.0.3:1", "oginstagram.com", "https://oginstagram.com"},
		{false, "172.30.0.3:1", "localhost", "https://localhost"},
	} {
		r := publicRequest("GET", "/.well-known/webfinger?resource=acct:alice@"+tt.host)
		r.RemoteAddr, r.Host, g.cfg.Development = tt.peer, tt.host, tt.dev
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"`+tt.want+`/alice"`) {
			t.Errorf("dev=%v host=%s: %d %s, want origin %s", tt.dev, tt.host, w.Code, w.Body.String(), tt.want)
		}
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
	// Direct hosts and offload media paths redirect to the media; gallery hosts (never
	// a client query) blank the description. Offload signatures are checked in ServeHTTP.
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
		{"oginstagram.com", "/alice?__gallery=1", "https://www.instagram.com/alice/", "profile", "", false},
		{"g.oginstagram.com", "/alice", "https://www.instagram.com/alice/", "profile", "", true},
		{"oginstagram.com", "/offload/ABC123/2", "", "", cdn + "2.jpg", false},
		{"oginstagram.com", "/offload/ABC123/2?thumbnail=1", "", "", cdn + "t2.jpg", false},
		{"oginstagram.com", "/offload/@Alice/1", "", "", cdn + "r1.jpg", false},
	} {
		r := publicRequest("GET", tt.path)
		r.Host = tt.host
		got, ok := resolveGatewayRoute(r)
		if !ok || got.humanRedirect != tt.redirect || got.category != tt.category {
			t.Fatalf("route %s %s = %+v,%v", tt.host, tt.path, got, ok)
		}
		res := got.handle(g.app, withOrigin(r))
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
	for _, tt := range []struct {
		method, path, want string
		status             int
	}{
		{"GET", "/stories/alice/123", `og:video:type`, 200},
		{"GET", "/alice", `og:type" content="profile`, 200},
		{"GET", "/p/ABC123", `og:video:type`, 200},
		{"GET", "/api/v1/statuses/123", `not found`, 404},
		{"GET", "/users/alice", `not found`, 404},
		{"GET", "/alice/outbox", `OrderedCollection`, 200},
		{"GET", "/p/GONE12", `Post unavailable`, 200},
		{"GET", "/bob", `Account unavailable`, 200},
		{"POST", "/alice/inbox", ``, 405},
		{"GET", "/stories/alice/123", `id="discord:component-embed"`, 200},
		{"GET", "/alice", `id="discord:component-embed"`, 200},
		{"GET", "/p/ABC123?e=s", `https://ko-fi.com/seirenkr`, 200},
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
		query, edge string
		status      int
	}{
		{"", "max-age=60", 404},
		{valid.RawQuery + "&sig=duplicate", "max-age=86400", 404},
		{strings.Replace(valid.RawQuery, "v=2", "v=3", 1), "max-age=86400", 404},
		// A removed key may be restored, so its links must not stay dead at the edge.
		{strings.Replace(valid.RawQuery, "kid=test-v1", "kid=retired", 1), "max-age=60", 404},
		{valid.RawQuery, "", 302},
	} {
		r := publicRequest("GET", "/offload/ABC123/1?"+tt.query)
		r.Header.Set("User-Agent", "Discordbot")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("offload got%d want%d body%s", w.Code, tt.status, w.Body.String())
		}
		if edge := w.Header().Get("Cloudflare-CDN-Cache-Control"); tt.status == 404 && edge != tt.edge {
			t.Errorf("offload %q edge cache = %q, want %q", tt.query, edge, tt.edge)
		}
		if _, warmed := g.app.posts.localGet("ABC123"); tt.status == 404 && warmed {
			t.Fatalf("rejected offload %q loaded the model", tt.query)
		}
	}
}

func TestGatewayOffloadErrorRedirectIsNotEdgeCached(t *testing.T) {
	g := testGateway(t)
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{err: ephemeralErr(504, errorCodeConnection, "timeout"), expiresAt: time.Now().Add(time.Minute)})
	signed, _ := url.Parse(g.app.offloadSigner.url("https://oginstagram.com", "/offload/ABC123/1", false))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, publicRequest("GET", signed.RequestURI()))
	if w.Code != 302 || w.Header().Get("Cloudflare-CDN-Cache-Control") != "" {
		t.Fatalf("error fallback = %d, edge cache %q", w.Code, w.Header().Get("Cloudflare-CDN-Cache-Control"))
	}
}

// serveAsset answers these before the router. The browser check does not
// mirror the asset list, so they stay out of shared/route-cases.json.
func TestPreviewRejectsAssetPaths(t *testing.T) {
	r := publicRequest("POST", "/api/embed")
	for _, path := range []string{"/robots.txt", "/sitemap.xml", "/.env", "/home/p/ABC123", "/assets/p/ABC123", "/preview/reel/ABC123/2"} {
		if _, _, ok := previewTarget(r, path); ok {
			t.Errorf("asset path accepted: %s", path)
		}
	}
}

func TestPreviewRejectsBadOriginAndDuplicateClearance(t *testing.T) {
	g := testGateway(t)
	for _, tt := range []struct{ origin, cookie, site string }{
		{"https://attacker.example", "cf_clearance=a", "same-origin"},
		{"https://attacker.example", "cf_clearance=a", ""},
		{"https://oginstagram.com", "", "same-origin"},
		{"https://oginstagram.com", "cf_clearance=a; cf_clearance=b", "same-origin"},
		{"https://oginstagram.com", "cf_clearance=a", "same-site"},
	} {
		r := publicRequest("POST", "/api/embed")
		r.Body = io.NopCloser(strings.NewReader(`{"path":"/p/ABC123"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", tt.site)
		r.Header.Set("Origin", tt.origin)
		r.Header.Set("Cookie", tt.cookie)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("preview got%d for %+v", w.Code, tt)
		}
	}
}

func TestPreviewResponses(t *testing.T) {
	g := testGateway(t)
	g.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":false,"action":"turnstile-spin-v1","hostname":"oginstagram.com"}`))}, nil
	})}
	post := Post{Shortcode: "ABC123", Username: "alice", Caption: "a <b>", Attachments: []Attachment{{URL: "https://scontent.cdninstagram.com/photo.jpg", Kind: "image"}}}
	for _, tt := range []struct {
		err    *AppError
		body   string
		status int
	}{
		{nil, `{"path":"/p/ABC123"}`, 200},
		// Instagram's 429 is not the visitor's rate limit.
		{igErr(429, errorCodeRateLimited, "limited"), `{"path":"/p/ABC123"}`, 502},
		{ephemeralErr(499, "", "cancelled"), `{"path":"/p/ABC123"}`, 504},
		{igErr(404, errorCodeNotFound, "gone"), `{"path":"/p/ABC123"}`, 404},
		{igErr(403, errorCodeRestricted, "restricted"), `{"path":"/p/ABC123"}`, 451},
		{nil, `{"path":"/p/ABC123","token":"token"}`, 403},
	} {
		g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{value: post, err: tt.err, expiresAt: time.Now().Add(time.Hour)})
		r := publicRequest("POST", "/api/embed")
		r.Body = io.NopCloser(strings.NewReader(tt.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://oginstagram.com")
		r.Header.Set("Cookie", "cf_clearance=test")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Errorf("preview %s with %v = %d, want %d", tt.body, tt.err, w.Code, tt.status)
		}
		if w.Code != 200 {
			continue
		}
		found := false
		for _, display := range componentsOfType(componentPayload(t, w.Body.String()), 10) {
			found = found || display["content"] == `a \<b>`
		}
		if !found || strings.Contains(w.Body.String(), `id="og-status"`) {
			t.Errorf("preview must carry the card's Component Embed caption: %s", w.Body.String())
		}
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
		path, peer string
		status     int
	}{{"/readyz", "127.0.0.1:2000", 200}, {"/readyz", "172.30.0.3:2000", 404}, {"/healthz", "127.0.0.1:2000", 403}} {
		r := publicRequest("GET", tt.path)
		r.RemoteAddr = tt.peer
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tt.status {
			t.Fatalf("%s from %s got%d want%d", tt.path, tt.peer, w.Code, tt.status)
		}
	}
}

func TestGatewayRejectsEncodedProtectedEndpoints(t *testing.T) {
	g := testGateway(t)
	for _, path := range []string{"/api%2Fembed", "/%61pi/embed", "/api/%65mbed", "/api/%73tatus"} {
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

func TestOffloadEdgeTTL(t *testing.T) {
	now := time.Now()
	cdn := "https://scontent.cdninstagram.com/v/x.jpg?oe=" + strconv.FormatInt(now.Add(3*time.Hour).Unix(), 16)
	link := func(exp time.Duration) *url.URL {
		u, _ := url.Parse("https://oginstagram.com/offload/ABC/1?exp=" + strconv.FormatInt(now.Add(exp).Unix(), 10))
		return u
	}
	if got := offloadEdgeTTL(link(14*24*time.Hour), cdn, now); got < 8990 || got > 9000 {
		t.Errorf("TTL must stop 30 minutes before the CDN URL expires: %d", got)
	}
	if got := offloadEdgeTTL(link(time.Hour), cdn, now); got > 3600 {
		t.Errorf("TTL must not outlive the signed link: %d", got)
	}
}

func TestDirectEdgeTTL(t *testing.T) {
	now := time.Now()
	oe := func(d time.Duration) string { return "?oe=" + strconv.FormatInt(now.Add(d).Unix(), 16) }
	cdn := "https://scontent.cdninstagram.com/v/x.jpg"
	for _, tt := range []struct {
		location string
		min, max int
	}{
		{cdn + oe(100*time.Hour), 86400, 86400},
		{cdn + oe(3*time.Hour), 8999, 9000},
		{cdn + oe(20*time.Minute), 0, 0},
		{cdn, 0, 0},
		{"https://d.oginstagram.com" + defaultAvatarPath, 0, 0},
		{"https://www.instagram.com/p/ABC123/", 0, 0},
		{"https://www.instagram.com/p/ABC123/" + oe(3*time.Hour), 0, 0},
		{"://scontent.cdninstagram.com/x.jpg" + oe(3*time.Hour), 0, 0},
	} {
		if got := directEdgeTTL(tt.location, now); got < tt.min || got > tt.max {
			t.Errorf("directEdgeTTL(%s) = %d, want %d..%d", tt.location, got, tt.min, tt.max)
		}
	}
}

var edgeCacheControl = regexp.MustCompile(`^max-age=([0-9]+)(, must-revalidate)?$`)

func TestGatewayEdgeCachesOnlyURLKeyedSuccesses(t *testing.T) {
	g := testGateway(t)
	now := time.Now()
	exp := now.Add(time.Hour)
	cdn := "https://scontent.cdninstagram.com/"
	oe := "?oe=" + strconv.FormatInt(now.Add(3*time.Hour).Unix(), 16)
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{value: Post{Shortcode: "ABC123", Username: "alice", Attachments: []Attachment{{Kind: "image", URL: cdn + "1.jpg" + oe}}}, expiresAt: exp})
	g.app.posts.storeLocal("GONE12", &cacheEntry[Post]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: exp})
	g.app.posts.storeLocal("SLOW12", &cacheEntry[Post]{err: ephemeralErr(504, errorCodeConnection, "timeout"), expiresAt: exp})
	g.app.posts.storeLocal("PART12", &cacheEntry[Post]{value: Post{Shortcode: "PART12", Username: "alice", Partial: true, Attachments: []Attachment{{Kind: "image", URL: cdn + "1.jpg" + oe}}}, expiresAt: exp})
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{value: Story{ID: "123", Username: "alice", Media: Attachment{Kind: "image", URL: cdn + "s.jpg" + oe}}, expiresAt: exp})
	for _, name := range []string{"alice", "al.js"} {
		g.app.profiles.storeLocal(name, &cacheEntry[Profile]{value: Profile{Username: name}, expiresAt: exp})
	}
	signed, _ := url.Parse(g.app.offloadSigner.url("https://oginstagram.com", "/offload/ABC123/1", false))
	partial, _ := url.Parse(g.app.offloadSigner.url("https://oginstagram.com", "/offload/PART12/1", false))
	for _, tt := range []struct {
		method, host, target string
		navigate             bool
		status, maxAge       int // maxAge 0: no Cloudflare-CDN-Cache-Control
		revalidate           bool
	}{
		{"GET", "d.oginstagram.com", "/p/ABC123", false, 302, 9000, true},
		{"GET", "d.oginstagram.com", "/stories/alice/123", false, 302, 9000, true},
		{"GET", "oginstagram.com", "/p/ABC123/", false, 200, cardEdgeTTL, false},
		{"GET", "g.oginstagram.com", "/reel/ABC123/?img_index=1", false, 200, cardEdgeTTL, false},
		{"GET", "oginstagram.com", "/stories/alice/123", false, 200, cardEdgeTTL, false},
		{"GET", "g.oginstagram.com", "/alice/outbox", false, 200, 0, false},
		{"GET", "oginstagram.com", signed.RequestURI(), false, 302, 9000, false},
		{"GET", "d.oginstagram.com", "/p/ABC123?thumbnail=1", false, 302, 0, false}, // default avatar
		{"GET", "d.oginstagram.com", "/p/SLOW12", false, 302, 0, false},             // fallback to Instagram
		{"GET", "d.oginstagram.com", "/p/GONE12", false, 404, 0, false},
		{"GET", "d.oginstagram.com", "/alice", false, 200, 0, false},
		{"HEAD", "oginstagram.com", "/p/ABC123/", false, 200, 0, false},
		{"GET", "oginstagram.com", "/p/GONE12/", false, 200, 0, false},
		{"GET", "oginstagram.com", "/p/SLOW12/", false, 200, 0, false},
		// A full post replaces a partial one; its card and cover links stay off the edge.
		{"GET", "oginstagram.com", "/p/PART12/", false, 200, 0, false},
		{"GET", "d.oginstagram.com", "/p/PART12", false, 302, 0, false},
		{"GET", "oginstagram.com", partial.RequestURI(), false, 302, 0, false},
		{"GET", "oginstagram.com", "/p/ABC123/", true, 307, 0, false},
		{"GET", "oginstagram.com", "/alice", false, 200, 0, false},
		{"GET", "oginstagram.com", "/al.js", false, 200, 0, false},
	} {
		r := publicRequest(tt.method, tt.target)
		r.Host = tt.host
		if tt.navigate {
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			r.Header.Set("Sec-Fetch-Dest", "document")
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		edge := w.Header().Get("Cloudflare-CDN-Cache-Control")
		m := edgeCacheControl.FindStringSubmatch(edge)
		age := 0
		if m != nil {
			age, _ = strconv.Atoi(m[1])
		}
		if w.Code != tt.status || w.Header().Get("Cache-Control") != "no-store" ||
			tt.maxAge == 0 && edge != "" || tt.maxAge > 0 && (m == nil || age < tt.maxAge-2 || age > tt.maxAge || (m[2] != "") != tt.revalidate) {
			t.Errorf("%s %s%s = %d, Cache-Control %q, Cloudflare-CDN-Cache-Control %q", tt.method, tt.host, tt.target, w.Code, w.Header().Get("Cache-Control"), edge)
		}
	}
}

func TestGatewayServesAvatarsFromOrigin(t *testing.T) {
	g := testGateway(t)
	now := time.Now()
	exp := now.Add(time.Hour)
	cdn := "https://scontent.cdninstagram.com/"
	oe := "?oe=" + strconv.FormatInt(now.Add(3*time.Hour).Unix(), 16)
	jpeg := "\xFF\xD8\xFF avatar"
	var fetched []string
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.Host+r.URL.Path)
		if !trustedMediaURL(r.URL) {
			t.Errorf("fetched untrusted %s", r.URL)
		}
		reply := func(status int, contentType, body string) (*http.Response, error) {
			h := http.Header{"Content-Type": {contentType}}
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		switch r.URL.Path {
		case "/pa.jpg":
			return reply(200, "image/jpeg", jpeg)
		case "/a.png":
			return reply(200, "image/png; charset=binary", "png")
		case "/sa.webp":
			return reply(200, "image/webp", "webp")
		case "/page.jpg":
			return reply(200, "text/html", "<html>")
		case "/big.jpg":
			return reply(200, "image/jpeg", strings.Repeat("x", maxAvatarBytes+1))
		case "/away.jpg":
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://attacker.example/x.jpg"}}, Body: http.NoBody}, nil
		case "/slow.jpg":
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return reply(404, "text/plain", "not found")
	}))
	posts := map[string]string{"ABC123": cdn + "pa.jpg" + oe, "PAGE12": cdn + "page.jpg", "BIG123": cdn + "big.jpg", "GONE12": cdn + "gone.jpg",
		"AWAY12": cdn + "away.jpg", "FOREIG": "https://cdn.example/avatar.jpg", "NOPIC1": ""}
	for code, pic := range posts {
		g.app.posts.storeLocal(code, &cacheEntry[Post]{value: Post{Shortcode: code, Username: "alice", ProfilePic: pic, Attachments: []Attachment{{Kind: "image", URL: cdn + "1.jpg" + oe}}}, expiresAt: exp})
	}
	g.app.profiles.storeLocal("alice", &cacheEntry[Profile]{value: Profile{Username: "alice", ProfilePic: cdn + "a.png" + oe, RecentMedia: []ProfileMedia{{Thumbnail: cdn + "r1.jpg"}}}, expiresAt: exp})
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{value: Story{ID: "123", Username: "alice", ProfilePic: cdn + "sa.webp", Media: Attachment{Kind: "image", URL: cdn + "s.jpg"}}, expiresAt: exp})
	for _, tt := range []struct {
		method, path, contentType, body, location string
		maxAge                                    int // 0: no Cloudflare-CDN-Cache-Control
		fetch                                     bool
	}{
		{"GET", "/offload/ABC123/avatar", "image/jpeg", jpeg, "", 9000, true},
		{"HEAD", "/offload/ABC123/avatar", "image/jpeg", "", "", 9000, true},
		{"GET", "/offload/@alice/avatar", "image/png", "png", "", 9000, true},
		{"GET", "/offload/@alice", "image/png", "png", "", 9000, true},
		{"GET", "/offload/story/alice/123/avatar", "image/webp", "webp", "", 86400, true},
		// A failed copy still redirects bots, and the edge asks again within a minute.
		{"GET", "/offload/PAGE12/avatar", "", "", cdn + "page.jpg", 60, true},
		{"GET", "/offload/BIG123/avatar", "", "", cdn + "big.jpg", 60, true},
		{"GET", "/offload/GONE12/avatar", "", "", cdn + "gone.jpg", 60, true},
		{"GET", "/offload/AWAY12/avatar", "", "", cdn + "away.jpg", 60, true},
		{"GET", "/offload/FOREIG/avatar", "", "", "https://cdn.example/avatar.jpg", 86400, false},
		{"GET", "/offload/NOPIC1/avatar", "", "", "https://oginstagram.com" + defaultAvatarPath, 86400, false},
		{"GET", "/offload/ABC123/1", "", "", cdn + "1.jpg" + oe, 9000, false},
		{"GET", "/offload/@alice/1", "", "", cdn + "r1.jpg", 86400, false},
		{"GET", "/offload/story/alice/123", "", "", cdn + "s.jpg", 86400, false},
	} {
		fetched = nil
		signed, _ := url.Parse(g.app.offloadSigner.url("https://oginstagram.com", tt.path, false))
		r := publicRequest(tt.method, signed.RequestURI())
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		edge := w.Header().Get("Cloudflare-CDN-Cache-Control")
		age := -1
		if m := edgeCacheControl.FindStringSubmatch(edge); m != nil {
			age, _ = strconv.Atoi(m[1])
		}
		status := 302
		if tt.contentType != "" {
			status = 200
		}
		if w.Code != status || w.Header().Get("Location") != tt.location || w.Body.String() != tt.body ||
			tt.contentType != "" && w.Header().Get("Content-Type") != tt.contentType || w.Header().Get("Cache-Control") != "no-store" ||
			tt.maxAge == 0 && edge != "" || tt.maxAge > 0 && (age < tt.maxAge-2 || age > tt.maxAge) || (len(fetched) > 0) != tt.fetch {
			t.Errorf("%s %s = %d %q %v %.40q (fetched %v)", tt.method, tt.path, w.Code, edge, w.Header(), w.Body.String(), fetched)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if res := avatarResp(ctx, cdn+"slow.jpg"); res.status != 302 || res.headers["Location"] != cdn+"slow.jpg" || res.originStatus != http.StatusGatewayTimeout || res.headers["Cloudflare-CDN-Cache-Control"] != "max-age=60" {
		t.Errorf("slow avatar = %d %v origin %d", res.status, res.headers, res.originStatus)
	}
}

func TestBotLimiterAllowsFiftyPerHour(t *testing.T) {
	var l botLimiter
	start := time.Date(2026, 10, 7, 5, 0, 0, 0, time.UTC)
	for i := range botHourlyLimit {
		if ok, _ := l.allow("198.51.100.7", start.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("request %d denied", i+1)
		}
	}
	ok, wait := l.allow("198.51.100.7", start.Add(10*time.Minute))
	if ok || wait != 50*time.Minute {
		t.Fatalf("51st request = %v, retry after %s; want denied for 50m", ok, wait)
	}
	if ok, _ := l.allow("198.51.100.8", start.Add(10*time.Minute)); !ok {
		t.Fatal("another IP has its own budget")
	}
	if ok, _ := l.allow("198.51.100.7", start.Add(time.Hour)); !ok {
		t.Fatal("the next hour starts a fresh window")
	}
}

func TestBotLimiterSharesOneBudgetPerIPv6Slash64(t *testing.T) {
	var l botLimiter
	now := time.Date(2026, 10, 7, 5, 0, 0, 0, time.UTC)
	for i := range botHourlyLimit {
		l.allow("2001:db8:1:2::"+strconv.FormatInt(int64(i+1), 16), now)
	}
	if ok, _ := l.allow("2001:db8:1:2:ffff::1", now); ok {
		t.Fatal("a fresh address in the same /64 got its own budget")
	}
	if ok, _ := l.allow("2001:db8:1:3::1", now); !ok {
		t.Fatal("another /64 has its own budget")
	}
}

func TestGatewayRateLimitsOnlyLimitedBots(t *testing.T) {
	g := testGateway(t)
	now := time.Now()
	for range botHourlyLimit {
		g.bots.allow("198.51.100.7", now)
	}
	r := publicRequest(http.MethodGet, "/p/DaD8phTyclR")
	r.Header.Set(botClassHeader, "limited")
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over-limit bot = %d (Retry-After %q), want 429", w.Code, w.Header().Get("Retry-After"))
	}
	// Exempt traffic (verified bots, Discord, partners) from the same IP passes on
	// to normal handling; an unsigned offload link answers 404 without any fetch.
	for class, want := range map[string]int{"exempt": http.StatusNotFound, "": http.StatusNotFound, "limited": http.StatusTooManyRequests} {
		r = publicRequest(http.MethodGet, "/offload/DaD8phTyclR/1")
		r.Header.Set(botClassHeader, class)
		r.Header.Set("CF-Connecting-IP", "198.51.100.7")
		w = httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("class %q = %d, want %d", class, w.Code, want)
		}
	}
}

func TestRecordMetric(t *testing.T) {
	g := &Gateway{metrics: make(chan metricEvent, 1)}
	posts := gatewayRoute{category: "posts"}
	g.recordMetric(posts, resp{status: 200, cacheHit: true}, time.Second)
	g.recordMetric(posts, resp{status: 200, originStatus: 504, errorType: errorCodeConnection}, time.Second)
	g.recordMetric(posts, resp{status: 200}, time.Second) // the backlog is full: dropped, not blocked
	if got := <-g.metrics; got.status != 504 || got.errorType != errorCodeConnection || len(g.metrics) != 0 {
		t.Fatalf("recorded %+v with %d more queued; want only the origin 504", got, len(g.metrics))
	}
}
