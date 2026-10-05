package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxGatewayInFlight = 256

var gatewayBotPattern = regexp.MustCompile(`(?i)bot|facebook|whatsapp|embed|got|firefox/92|curl|wget|go-http|yahoo|generator|revoltchat|preview|link|proxy|vkshare|images|analyzer|index|crawl|spider|python|node|deno|mastodon|http\.rb|ruby|bun/|fiddler|iframely|bluesky|matrix|cardyb|resolver|feedly|rss|reader|atom|thunderbird|axios`)
var statusCodePattern = regexp.MustCompile(`^[0-9]{1,256}$`)

func gatewayIsBot(ua string) bool { return gatewayBotPattern.MatchString(ua) }

type Gateway struct {
	cfg    Config
	app    *App
	home   map[string]string // locale → home page template
	slots  chan struct{}
	client *http.Client

	metrics chan metricEvent

	statusMu   sync.Mutex
	statusBody []byte
	statusAt   time.Time
}

type metricEvent struct {
	category, errorType string
	duration            time.Duration
	status              int
}

func newGateway(cfg Config, app *App, home map[string]string) *Gateway {
	g := &Gateway{cfg: cfg, app: app, home: home, slots: make(chan struct{}, maxGatewayInFlight),
		client: &http.Client{Timeout: 10 * time.Second}, metrics: make(chan metricEvent, 256)}
	go g.writeMetrics()
	return g
}

// One writer keeps SQLite metric inserts off every response's critical path.
func (g *Gateway) writeMetrics() {
	for m := range g.metrics {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := g.cfg.Store.RecordMetric(ctx, m.category, m.duration, m.status, m.errorType); err != nil {
			slog.Warn("metric write failed", "error", err)
		}
		cancel()
	}
}

func peerAddress(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, _ := netip.ParseAddr(host)
	return addr.Unmap()
}

func (g *Gateway) trustedPeer(r *http.Request) bool {
	peer := peerAddress(r)
	for _, p := range g.cfg.TrustedProxies {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

func (g *Gateway) publicOrigin(r *http.Request) (string, bool) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if g.cfg.Development && peerAddress(r).IsLoopback() && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return "http://" + r.Host, true
	}
	for _, allowed := range g.cfg.AllowedHosts {
		if host == allowed && r.Host == host {
			return "https://" + host, true
		}
	}
	return "", false
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setPublicSecurityHeaders(w.Header())
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		if !peerAddress(r).IsLoopback() {
			g.problem(w, r, 404, "not found")
			return
		}
		if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		if r.URL.Path == "/readyz" {
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			if g.cfg.Store.Healthy(ctx) != nil {
				g.problem(w, r, 503, "storage unavailable")
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte("ok\n"))
		}
		return
	}
	if !g.trustedPeer(r) && !(g.cfg.Development && peerAddress(r).IsLoopback()) {
		g.problem(w, r, 403, "forbidden")
		return
	}
	origin, ok := g.publicOrigin(r)
	if !ok {
		g.problem(w, r, 421, "unrecognized host")
		return
	}
	// Cloudflare's preview WAF rule matches the public URI path. Reject encoded
	// aliases rather than dispatching a decoded path that escaped that rule.
	if (r.URL.Path == "/api/embed" || r.URL.Path == "/api/status" || r.URL.Path == "/api/admin/purge") && r.URL.EscapedPath() != r.URL.Path {
		g.problem(w, r, 404, "not found")
		return
	}
	requestID := rand.Text()
	if ray := r.Header.Get("Cf-Ray"); len(ray) > 0 && len(ray) <= 128 {
		requestID = ray
	}
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
	if r.URL.Path == "/" {
		if allowMethod(w, r, "GET", "HEAD") {
			g.serveHome(w, r)
		}
		return
	}
	if g.serveAsset(w, r) {
		return
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		g.problem(w, r, 503, "overloaded")
		return
	}
	switch r.URL.Path {
	case "/api/embed":
		g.serveEmbed(w, r, origin)
		return
	case "/api/status":
		g.serveStatus(w, r)
		return
	case "/api/admin/purge":
		g.servePurge(w, r)
		return
	}
	if !allowMethod(w, r, "GET", "HEAD") {
		return
	}
	route, ok := resolveGatewayRoute(r)
	if !ok {
		g.problem(w, r, 404, "not found")
		return
	}
	// Cloudflare Redirect Rules send most browser navigations to Instagram before
	// the WAF. This catches the rest (e.g. /p/X/2/, challenge-passed visitors).
	if route.humanRedirect != "" && r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document" {
		http.Redirect(w, r, route.humanRedirect, http.StatusTemporaryRedirect)
		return
	}
	if route.offload && !g.app.offloadSigner.authorize(r.URL, time.Now()) {
		g.problem(w, r, 404, "not found")
		return
	}
	started := time.Now()
	result := g.render(r, route)
	elapsed := time.Since(started)
	g.recordMetric(r, route, result, elapsed)
	w.Header().Set("Server-Timing", "origin;dur="+strconv.FormatFloat(float64(elapsed.Microseconds())/1000, 'f', 2, 64))
	if route.offload && result.status == http.StatusFound && proxyOffloadMedia(w, r, result) {
		return
	}
	g.writeResult(w, r, result)
}

func allowMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	w.WriteHeader(http.StatusMethodNotAllowed)
	return false
}

func (g *Gateway) problem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	if status == http.StatusForbidden || status == http.StatusMisdirectedRequest || status >= 500 {
		slog.Warn("request rejected", "status", status, "detail", detail, "method", r.Method,
			"host", r.Host, "path", r.URL.Path, "peer", peerAddress(r).String(), "cf_ray", r.Header.Get("Cf-Ray"))
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})
	}
}

func (g *Gateway) writeResult(w http.ResponseWriter, r *http.Request, result resp) {
	for k, v := range result.headers {
		w.Header().Set(k, v)
	}
	// Dynamic responses always reach the gateway for authorization, UA and Accept.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(result.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(result.body)
	}
}

// handle gets render's request (same URL, plus the render timeout). Its
// arguments were validated by resolveGatewayRoute; handlers never re-match paths.
type gatewayRoute struct {
	handle        func(*App, *http.Request) resp
	humanRedirect string
	category      string
	offload       bool
}

func resolveGatewayRoute(r *http.Request) (gatewayRoute, bool) {
	segments := splitPath(r.URL.Path)
	route := gatewayRoute{}
	gallery := strings.HasPrefix(r.Host, "g.") || strings.HasPrefix(r.Host, "www.g.")
	direct := strings.HasPrefix(r.Host, "d.") || strings.HasPrefix(r.Host, "www.d.")
	if r.URL.Path == "/.well-known/webfinger" {
		route.handle = (*App).handleWebFinger
		return route, true
	}
	if len(segments) > 0 && segments[0] == "offload" {
		canonical, ok := canonicalOffloadPath(r.URL)
		if !ok {
			return route, false
		}
		route.handle = offloadHandler(splitPath(canonical))
		route.offload = true
		return route, true
	}
	if len(segments) == 3 && segments[0] == "stories" && validUsername(segments[1]) && validStoryID(segments[2]) {
		username, id := segments[1], segments[2]
		route.category = "stories"
		if direct {
			route.handle = func(a *App, r *http.Request) resp { return a.handleStoryOffload(r, username, id, false) }
		} else {
			route.humanRedirect = storyOriginURL(username, id)
			route.handle = func(a *App, r *http.Request) resp { return a.handleStory(r, username, id, gallery) }
		}
		return route, true
	}
	if len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "statuses" {
		code := segments[3]
		route.handle = func(a *App, r *http.Request) resp { return a.handleMastodonStatus(r, code) }
		return route, statusCodePattern.MatchString(code)
	}
	if len(segments) == 4 && segments[0] == "users" && validUsername(segments[1]) && segments[2] == "statuses" {
		code := segments[3]
		route.handle = func(a *App, r *http.Request) resp { return a.handleActivity(r, code) }
		return route, statusCodePattern.MatchString(code)
	}
	if len(segments) == 3 && segments[0] == "users" && validUsername(segments[1]) && (segments[2] == "inbox" || segments[2] == "outbox") {
		username, name := segments[1], segments[2]
		route.handle = func(a *App, r *http.Request) resp { return a.handleActivityCollection(r, username, name) }
		return route, true
	}
	if len(segments) == 2 && segments[0] == "users" && validUsername(segments[1]) {
		username := segments[1]
		route.handle = func(a *App, r *http.Request) resp {
			return activityJSONResp(http.StatusOK, a.buildFallbackAccount(a.publicBaseURL(r), username))
		}
		return route, true
	}
	if post := parseEmbedSegments(segments); post != nil {
		route.category = "posts"
		index, specified := mediaSelection(r.URL.Query(), post.PathIndex)
		if direct {
			route.handle = func(a *App, r *http.Request) resp { return a.handleOffload(r, post.Shortcode, index, false) }
		} else {
			route.humanRedirect = instagramPostURL(post.PostType, post.Shortcode, index, specified)
			route.handle = func(a *App, r *http.Request) resp {
				return a.handlePost(r, post.PostType, post.Shortcode, index, specified, gallery)
			}
		}
		return route, true
	}
	if len(segments) == 1 && validUsername(segments[0]) {
		username := segments[0]
		route.category = "profile"
		route.humanRedirect = profileURL(username)
		route.handle = func(a *App, r *http.Request) resp { return a.handleProfile(r, username, gallery) }
		return route, true
	}
	return route, false
}

func canonicalOffloadPath(u *url.URL) (string, bool) {
	segments := splitPath(u.Path)
	if len(segments) < 2 || segments[0] != "offload" || u.EscapedPath() != "/"+strings.Join(segments, "/") {
		return "", false
	}
	if segments[1] == "story" && (len(segments) == 4 || len(segments) == 5) {
		if !validUsername(segments[2]) || !validStoryID(segments[3]) || (len(segments) == 5 && segments[4] != "avatar") {
			return "", false
		}
		segments[2] = strings.ToLower(segments[2])
	} else {
		if len(segments) != 2 && len(segments) != 3 {
			return "", false
		}
		maxIndex := maxCachedMediaItems
		if username, ok := strings.CutPrefix(segments[1], "@"); ok {
			if !validUsername(username) {
				return "", false
			}
			segments[1] = "@" + strings.ToLower(username)
			maxIndex = profileGalleryMax
		} else if !validShortcode(segments[1]) {
			return "", false
		}
		if len(segments) == 3 && segments[2] != "avatar" {
			n, ok := parseCanonicalDecimal(segments[2])
			if !ok || n < 1 || n > maxIndex {
				return "", false
			}
		}
	}
	return "/" + strings.Join(segments, "/"), true
}

// offloadHandler dispatches canonicalOffloadPath's segments, whose usernames
// are lowercased and whose media number is already within range.
func offloadHandler(s []string) func(*App, *http.Request) resp {
	if s[1] == "story" && len(s) >= 4 {
		return func(a *App, r *http.Request) resp { return a.handleStoryOffload(r, s[2], s[3], len(s) == 5) }
	}
	avatar := len(s) == 3 && s[2] == "avatar"
	n := 0
	if len(s) == 3 && !avatar {
		n, _ = parseCanonicalDecimal(s[2])
	}
	if username, ok := strings.CutPrefix(s[1], "@"); ok {
		return func(a *App, r *http.Request) resp { return a.handleProfileOffload(r, username, n) }
	}
	return func(a *App, r *http.Request) resp { return a.handleOffload(r, s[1], max(n-1, 0), avatar) }
}

func (s offloadSigner) authorize(u *url.URL, now time.Time) bool {
	path, ok := canonicalOffloadPath(u)
	if !ok {
		return false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, name := range []string{"v", "kid", "exp", "sig"} {
		if len(values[name]) != 1 {
			return false
		}
	}
	if values.Get("v") != offloadSignatureVersion {
		return false
	}
	kid := values.Get("kid")
	key := s.keys[kid]
	if len(key) != offloadSigningKeySize {
		return false
	}
	expires, err := strconv.ParseInt(values.Get("exp"), 10, 64)
	if err != nil || strconv.FormatInt(expires, 10) != values.Get("exp") || expires <= now.Unix() || expires > now.Unix()+int64(offloadCapabilityTTL/time.Second) {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(values.Get("sig"))
	if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != values.Get("sig") {
		return false
	}
	return hmac.Equal(signature, offloadMAC(key, kid, path, expires))
}

func (g *Gateway) render(r *http.Request, route gatewayRoute) resp {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	return route.handle(g.app, r.WithContext(ctx))
}

// previewTarget turns the client-supplied path into the internal bot GET the
// preview renders. The gateway router decides which shapes are embeddable.
func previewTarget(r *http.Request, path string) (*http.Request, gatewayRoute, bool) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "#\\") || strings.IndexFunc(path, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return nil, gatewayRoute{}, false
	}
	u, err := url.ParseRequestURI(path)
	if err != nil || u.Host != "" || u.Scheme != "" {
		return nil, gatewayRoute{}, false
	}
	internal := r.Clone(r.Context())
	internal.Method = http.MethodGet
	internal.URL = u
	internal.Header.Set("User-Agent", "OGInstagramPreviewBot/1.0")
	route, ok := resolveGatewayRoute(internal)
	if !ok || route.category == "" || !previewQuery(u.RawQuery, route.category == "posts") {
		return nil, gatewayRoute{}, false
	}
	return internal, route, true
}

// Only posts take a query, and only a single canonical img_index.
func previewQuery(raw string, post bool) bool {
	if raw == "" {
		return true
	}
	query, err := url.ParseQuery(raw)
	if err != nil || !post || len(query) != 1 || len(query["img_index"]) != 1 {
		return false
	}
	n, ok := parseCanonicalDecimal(query.Get("img_index"))
	return ok && n > 0 && n <= maxCachedMediaItems
}

func (g *Gateway) serveEmbed(w http.ResponseWriter, r *http.Request, origin string) {
	if !allowMethod(w, r, "POST") {
		return
	}
	// Siteverify can take ten seconds before the four-second render begins.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Second))
	if r.Header.Get("Origin") != origin || r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		g.problem(w, r, 403, "forbidden")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		g.problem(w, r, 415, "application/json required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Path  string          `json:"path"`
		Token json.RawMessage `json:"token"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			g.problem(w, r, 413, "body too large")
		} else {
			g.problem(w, r, 400, "invalid request")
		}
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		g.problem(w, r, 400, "invalid request")
		return
	}
	internal, route, ok := previewTarget(r, body.Path)
	if !ok {
		g.problem(w, r, 400, "invalid Instagram path")
		return
	}
	// nginx rate-limits previews per cf_clearance; a request without exactly one
	// clearance has not passed the edge challenge.
	if !hasClearance(r) && !(g.cfg.Development && peerAddress(r).IsLoopback()) {
		g.problem(w, r, 403, "forbidden")
		return
	}
	if body.Token != nil {
		var token string
		if json.Unmarshal(body.Token, &token) != nil || !g.verifyTurnstile(r, origin, token) {
			g.problem(w, r, 403, "forbidden")
			return
		}
	}
	started := time.Now()
	result := g.render(internal, route)
	g.recordMetric(internal, route, result, time.Since(started))
	if status := result.originStatus; status >= 400 && status <= 599 {
		if status == 429 {
			status = 502
		}
		if status == 499 {
			status = 504
		}
		g.problem(w, r, status, "preview unavailable")
		return
	}
	g.writeResult(w, r, result)
}

func hasClearance(r *http.Request) bool {
	values := r.CookiesNamed("cf_clearance")
	return len(values) == 1 && values[0].Value != "" && len(values[0].Value) <= 4096
}

func (g *Gateway) verifyTurnstile(r *http.Request, origin, token string) bool {
	if token == "" || len(token) > 2048 || g.cfg.TurnstileSecretKey == "" {
		return false
	}
	form := url.Values{"secret": {g.cfg.TurnstileSecretKey}, "response": {token}, "remoteip": {r.Header.Get("CF-Connecting-IP")}}
	req, err := http.NewRequestWithContext(r.Context(), "POST", "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := g.client.Do(req)
	if err != nil {
		slog.Warn("turnstile siteverify failed", "error", err)
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		slog.Warn("turnstile siteverify failed", "status", response.StatusCode)
		return false
	}
	var result struct {
		Success    bool     `json:"success"`
		Action     string   `json:"action"`
		Hostname   string   `json:"hostname"`
		ErrorCodes []string `json:"error-codes"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&result)
	page, _ := url.Parse(origin)
	if err != nil || !result.Success || result.Action != "turnstile-spin-v1" || result.Hostname != page.Hostname() {
		slog.Warn("turnstile rejected", "error_codes", result.ErrorCodes, "action", result.Action, "hostname", result.Hostname)
		return false
	}
	return true
}

func (g *Gateway) recordMetric(r *http.Request, route gatewayRoute, result resp, duration time.Duration) {
	// Only origin outcomes for bots: cache hits say nothing about upstream health.
	if route.category == "" || result.cacheHit || !gatewayIsBot(r.UserAgent()) {
		return
	}
	status := result.status
	if result.originStatus > 0 {
		status = result.originStatus
	}
	// Metrics are best-effort: a full backlog drops the sample, never the response.
	select {
	case g.metrics <- metricEvent{category: route.category, errorType: result.errorType, duration: duration, status: status}:
	default:
	}
}

func (g *Gateway) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, "GET", "HEAD") {
		return
	}
	// Holding the lock during the scan merges concurrent misses into one query.
	g.statusMu.Lock()
	if time.Since(g.statusAt) >= time.Minute {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		report, err := g.cfg.Store.Status(ctx)
		cancel()
		if err != nil {
			g.statusMu.Unlock()
			g.problem(w, r, 503, "status unavailable")
			return
		}
		g.statusBody, g.statusAt = jsonBytes(report), time.Now()
	}
	body := g.statusBody
	g.statusMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (g *Gateway) servePurge(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, "POST") {
		return
	}
	provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	actual, expected := sha256.Sum256([]byte(provided)), sha256.Sum256([]byte(g.cfg.AdminPurgeToken))
	if !ok || g.cfg.AdminPurgeToken == "" || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
		g.problem(w, r, 403, "forbidden")
		return
	}
	if err := g.cfg.Store.PurgeModels(r.Context()); err != nil {
		g.problem(w, r, 503, "local purge failed")
		return
	}
	g.app.posts.clear()
	g.app.profiles.clear()
	g.app.stories.clear()
	previewCache.clear()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
