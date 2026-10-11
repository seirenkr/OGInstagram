package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxGatewayInFlight = 256
	// Far above one hour of genuine limited-bot IPs; a flood of fresh addresses
	// is refused instead of growing the map without bound.
	maxBotLimiterKeys = 100_000
)

func validStatusCode(code string) bool {
	return code != "" && len(code) <= maxSnowcodeDigits && strings.Trim(code, "0123456789") == ""
}

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

	bots botLimiter
}

// botLimiter caps requests that Cloudflare classed as unverified bots (a Request
// Header Transform Rule sets botClassHeader on every request, so clients cannot
// forge it) to botHourlyLimit per visitor IP. The Free plan's rate limiting only
// counts over 10 s, so the hourly count lives here.
// ponytail: fixed clock-hour window per instance (up to 2x at the boundary,
// resets on deploy); move to a sliding window in SQLite if that ever matters.
type botLimiter struct {
	mu     sync.Mutex
	hour   int64
	counts map[string]int
}

// allow counts one request from ip, or reports how long until the next window.
// An IPv6 client usually holds a whole /64, so that is one visitor.
func (l *botLimiter) allow(ip string, now time.Time) (bool, time.Duration) {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		ip = netip.PrefixFrom(a, 64).Masked().String()
	}
	hour := now.Unix() / 3600
	l.mu.Lock()
	defer l.mu.Unlock()
	if hour != l.hour || l.counts == nil {
		l.hour, l.counts = hour, map[string]int{}
	}
	n, seen := l.counts[ip]
	if n >= botHourlyLimit || !seen && len(l.counts) >= maxBotLimiterKeys {
		return false, time.Unix((hour+1)*3600, 0).Sub(now)
	}
	l.counts[ip] = n + 1
	return true, 0
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

// One writer keeps the bucket-rollover SQLite flush off every response's critical path.
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
	if r.URL.Path == "/readyz" {
		if !peerAddress(r).IsLoopback() {
			g.problem(w, r, 404, "not found")
			return
		}
		if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := g.cfg.Store.Healthy(ctx); err != nil {
			g.problemCause(w, r, 503, "storage unavailable", err)
			return
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
	if (r.URL.Path == "/api/embed" || r.URL.Path == "/api/status") && r.URL.EscapedPath() != r.URL.Path {
		g.problem(w, r, 404, "not found")
		return
	}
	requestID := r.Header.Get("Cf-Ray")
	if requestID == "" || len(requestID) > 128 {
		requestID = rand.Text()
	}
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), requestIDKey{}, requestID), publicOriginKey{}, origin))
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
	if r.Header.Get(botClassHeader) == "limited" {
		if ok, wait := g.bots.allow(r.Header.Get("CF-Connecting-IP"), time.Now()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			g.problem(w, r, http.StatusTooManyRequests, "rate limited")
			return
		}
	}
	if route.offload && !g.app.offloadSigner.authorize(r.URL, time.Now()) {
		// A link that fails verification under a known key never becomes valid; let
		// the edge keep the 404. An unknown kid may be a key an operator restores.
		ttl := "60"
		if g.app.offloadSigner.keys[r.URL.Query().Get("kid")] != nil {
			ttl = "86400"
		}
		w.Header().Set("Cloudflare-CDN-Cache-Control", "max-age="+ttl)
		g.problem(w, r, 404, "not found")
		return
	}
	started := time.Now()
	result := g.render(r, route)
	// An error fallback (originStatus set) points at Instagram, not media, and
	// must not outlive the origin's own short error cache.
	if route.offload && result.status == http.StatusFound && result.originStatus == 0 {
		result.headers["Cloudflare-CDN-Cache-Control"] = "max-age=" + strconv.Itoa(offloadEdgeTTL(r.URL, result.headers["Location"], time.Now()))
	}
	elapsed := time.Since(started)
	g.recordMetric(route, result, elapsed)
	w.Header().Set("Server-Timing", "origin;dur="+strconv.FormatFloat(float64(elapsed.Microseconds())/1000, 'f', 2, 64))
	g.writeResult(w, r, result)
}

// offloadEdgeTTL lets Cloudflare answer repeat hits on a signed link until the
// CDN URL it redirects to nears expiry, and never past the link's own expiry.
func offloadEdgeTTL(link *url.URL, location string, now time.Time) int {
	ttl := cacheTTLFromURLs(location)
	if exp, err := strconv.ParseInt(link.Query().Get("exp"), 10, 64); err == nil {
		ttl = min(ttl, time.Unix(exp, 0).Sub(now))
	}
	return max(int(ttl/time.Second), 0)
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
	g.problemCause(w, r, status, detail, nil)
}

func (g *Gateway) problemCause(w http.ResponseWriter, r *http.Request, status int, detail string, cause error) {
	// nginx already logs every overload rejection; one Warn each would flood.
	if (status == http.StatusForbidden || status == http.StatusMisdirectedRequest || status >= 500) && detail != "overloaded" {
		l := logger(r.Context())
		if cause != nil {
			l = l.With("error", cause)
		}
		l.Warn("request rejected", "status", status, "detail", detail, "method", r.Method,
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
	// /stories/highlights/{id} is a highlight reel, not a story of the user "highlights".
	if len(segments) == 3 && segments[0] == "stories" && segments[1] != "highlights" && validUsername(segments[1]) && validStoryID(segments[2]) {
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
		return route, validStatusCode(code)
	}
	if len(segments) == 4 && segments[0] == "users" && validUsername(segments[1]) && segments[2] == "statuses" {
		code := segments[3]
		route.handle = func(a *App, r *http.Request) resp { return a.handleActivity(r, code) }
		return route, validStatusCode(code)
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
	if !ok || route.category == "" || reservedAssetPath(u.Path) || !previewQuery(u.RawQuery, route.category == "posts") {
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
	// Safari before 16.4 sends no Sec-Fetch-Site; the exact Origin and the JSON
	// content type still keep cross-origin requests out.
	if site := r.Header.Get("Sec-Fetch-Site"); r.Header.Get("Origin") != origin || site != "" && site != "same-origin" {
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
	g.recordMetric(route, result, time.Since(started))
	if status := result.originStatus; status >= 400 && status <= 599 {
		if status == 429 {
			status = 502
		}
		if status == 499 {
			status = 504
		}
		// A gated post gets the page's "unavailable" copy (404/451), not a generic error.
		if result.errorType == errorCodeRestricted {
			status = http.StatusUnavailableForLegalReasons
		}
		g.problem(w, r, status, "preview unavailable")
		return
	}
	// Inline the Mastodon status the card links to (an L1 hit by now), so the
	// page needs no second request that the bot limiter would count.
	if head, _, ok := bytes.Cut(result.body, []byte(`" rel="alternate" type="application/activity+json">`)); ok && result.status == http.StatusOK && result.originStatus == 0 {
		code := string(head[bytes.LastIndexByte(head, '/')+1:])
		if status := g.render(internal, gatewayRoute{handle: func(a *App, r *http.Request) resp { return a.handleMastodonStatus(r, code) }}); status.status == http.StatusOK {
			var escaped bytes.Buffer
			json.HTMLEscape(&escaped, status.body)
			result.body = append(result.body, `<script type="application/json" id="og-status">`+escaped.String()+`</script>`...)
		}
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

func (g *Gateway) recordMetric(route gatewayRoute, result resp, duration time.Duration) {
	// Only origin outcomes: cache hits say nothing about upstream health.
	if route.category == "" || result.cacheHit {
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
			g.problemCause(w, r, 503, "status unavailable", err)
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
