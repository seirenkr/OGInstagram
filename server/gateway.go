package main

import (
	"cmp"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxGatewayInFlight = 256
	maxBotLimiterKeys  = 100_000
)

type Gateway struct {
	cfg    Config
	app    *App
	home   map[string]string
	slots  chan struct{}
	client *http.Client

	metrics chan metricEvent

	statusMu   sync.Mutex
	statusBody []byte
	statusAt   time.Time

	bots botLimiter
}

type botLimiter struct {
	mu     sync.Mutex
	hour   int64
	counts map[string]int
}

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
	return slices.ContainsFunc(g.cfg.TrustedProxies, func(p netip.Prefix) bool { return p.Contains(peer) })
}

func (g *Gateway) publicOrigin(r *http.Request) (string, bool) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if g.cfg.Development && peerAddress(r).IsLoopback() && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return "http://" + r.Host, true
	}
	if r.Host == host && slices.Contains(g.cfg.AllowedHosts, host) {
		return "https://" + host, true
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
		if allowMethod(w, r, http.MethodGet, http.MethodHead) {
			g.writeResult(w, r, textResp(http.StatusOK, "ok\n"))
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
	// Cloudflare's preview WAF matches the raw URI path; reject encoded aliases.
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
	if route.offload && (result.status == http.StatusFound || result.status == http.StatusOK) && result.originStatus == 0 {
		result.headers["Cloudflare-CDN-Cache-Control"] = "max-age=" + strconv.Itoa(offloadEdgeTTL(r.URL, cmp.Or(result.headers["Location"], result.mediaURL), time.Now()))
	}
	if route.direct && result.status == http.StatusFound && result.originStatus == 0 {
		if ttl := directEdgeTTL(result.headers["Location"], time.Now()); ttl > 0 {
			result.headers["Cloudflare-CDN-Cache-Control"] = "max-age=" + strconv.Itoa(ttl) + ", must-revalidate"
		}
	}
	if route.card && r.Method == http.MethodGet && result.status == http.StatusOK && result.originStatus == 0 &&
		strings.HasPrefix(result.headers["Content-Type"], "text/html") {
		result.headers["Cloudflare-CDN-Cache-Control"] = "max-age=" + strconv.Itoa(cardEdgeTTL)
	}
	elapsed := time.Since(started)
	g.recordMetric(route, result, elapsed)
	w.Header().Set("Server-Timing", "origin;dur="+strconv.FormatFloat(float64(elapsed.Microseconds())/1000, 'f', 2, 64))
	g.writeResult(w, r, result)
}

func offloadEdgeTTL(link *url.URL, location string, now time.Time) int {
	ttl := cacheTTLFromURLs(location)
	if exp, err := strconv.ParseInt(link.Query().Get("exp"), 10, 64); err == nil {
		ttl = min(ttl, time.Unix(exp, 0).Sub(now))
	}
	return max(int(ttl/time.Second), 0)
}

func directEdgeTTL(location string, now time.Time) int {
	u, err := url.Parse(location)
	if err != nil || !trustedMediaURL(u) {
		return 0
	}
	expiry, ok := cdnExpiry(location)
	if !ok {
		return 0
	}
	return max(int(min(cdnFallbackTTL, expiry.Sub(now)-cdnTTLMargin)/time.Second), 0)
}

func allowMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	if slices.Contains(methods, r.Method) {
		return true
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	w.WriteHeader(http.StatusMethodNotAllowed)
	return false
}

func (g *Gateway) problem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	g.problemCause(w, r, status, detail, nil)
}

func (g *Gateway) problemCause(w http.ResponseWriter, r *http.Request, status int, detail string, cause error) {
	if (status == http.StatusForbidden || status == http.StatusMisdirectedRequest || status >= 500) && detail != "overloaded" {
		l := logger(r.Context())
		if cause != nil {
			l = l.With("error", cause)
		}
		l.Warn("request rejected", "status", status, "detail", detail, "method", r.Method,
			"host", r.Host, "path", r.URL.Path, "peer", peerAddress(r).String(), "cf_ray", r.Header.Get("Cf-Ray"))
	}
	g.writeResult(w, r, problemResp(status, detail))
}

func (g *Gateway) writeResult(w http.ResponseWriter, r *http.Request, result resp) {
	for k, v := range result.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(result.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(result.body)
	}
}

type gatewayRoute struct {
	handle        func(*App, *http.Request) resp
	humanRedirect string
	category      string
	offload       bool
	direct        bool
	// Profiles stay out: a dotted username looks like a file extension Cloudflare caches by default.
	card bool
}

func resolveGatewayRoute(r *http.Request) (gatewayRoute, bool) {
	segments := splitPath(r.URL.Path)
	route := gatewayRoute{}
	gallery := strings.HasPrefix(r.Host, "g.")
	direct := strings.HasPrefix(r.Host, "d.")
	if r.URL.Path == "/.well-known/webfinger" {
		route.handle = (*App).handleWebFinger
		return route, true
	}
	if len(segments) > 0 && segments[0] == "offload" {
		_, handle, ok := offloadRoute(r.URL)
		route.handle, route.offload = handle, true
		return route, ok
	}
	if username, id, ok := parseStorySegments(segments); ok {
		route.category, route.direct, route.card = "stories", direct, !direct
		if direct {
			route.handle = func(a *App, r *http.Request) resp { return a.handleStoryOffload(r, username, id, false) }
		} else {
			route.humanRedirect = storyOriginURL(username, id)
			route.handle = func(a *App, r *http.Request) resp { return a.handleStory(r, username, id, gallery) }
		}
		return route, true
	}
	if post := parseEmbedSegments(segments); post != nil {
		route.category, route.direct, route.card = "posts", direct, !direct
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
	if share := parseShareSegments(segments); share != nil {
		route.category, route.direct, route.card = "share", direct, !direct
		index, specified := mediaSelection(r.URL.Query(), share.PathIndex)
		if !direct {
			route.humanRedirect = instagramShareURL(share.Kind, share.Token, index, specified)
		}
		route.handle = func(a *App, r *http.Request) resp {
			return a.handleShare(r, *share, index, specified, direct)
		}
		return route, true
	}
	if len(segments) == 2 && isProfileUsername(segments[0]) && slices.Contains([]string{"inbox", "outbox", "followers", "following"}, segments[1]) {
		username, name := segments[0], segments[1]
		route.handle = func(a *App, r *http.Request) resp { return a.handleActivityCollection(r, username, name) }
		return route, true
	}
	if len(segments) == 1 && isProfileUsername(segments[0]) {
		username := segments[0]
		route.category = "profile"
		route.humanRedirect = profileURL(username)
		route.handle = func(a *App, r *http.Request) resp { return a.handleProfile(r, username, gallery) }
		return route, true
	}
	return route, false
}

func offloadRoute(u *url.URL) (string, func(*App, *http.Request) resp, bool) {
	s := splitPath(u.Path)
	if len(s) < 2 || s[0] != "offload" || u.EscapedPath() != "/"+strings.Join(s, "/") {
		return "", nil, false
	}
	var handle func(*App, *http.Request) resp
	if s[1] == "story" && (len(s) == 4 || len(s) == 5) {
		if !validUsername(s[2]) || !validStoryID(s[3]) || (len(s) == 5 && s[4] != "avatar") {
			return "", nil, false
		}
		s[2] = strings.ToLower(s[2])
		username, id, avatar := s[2], s[3], len(s) == 5
		handle = func(a *App, r *http.Request) resp { return a.handleStoryOffload(r, username, id, avatar) }
	} else {
		if len(s) != 2 && len(s) != 3 {
			return "", nil, false
		}
		avatar, n, maxIndex := len(s) == 3 && s[2] == "avatar", 0, maxCachedMediaItems
		username, profile := strings.CutPrefix(s[1], "@")
		if profile {
			if !validUsername(username) {
				return "", nil, false
			}
			username, maxIndex = strings.ToLower(username), profileGalleryMax
			s[1] = "@" + username
		} else if !validShortcode(s[1]) {
			return "", nil, false
		}
		if len(s) == 3 && !avatar {
			var ok bool
			if n, ok = parseCanonicalDecimal(s[2]); !ok || n < 1 || n > maxIndex {
				return "", nil, false
			}
		}
		shortcode := s[1]
		handle = func(a *App, r *http.Request) resp { return a.handleOffload(r, shortcode, max(n-1, 0), avatar) }
		if profile {
			handle = func(a *App, r *http.Request) resp { return a.handleProfileOffload(r, username, n) }
		}
	}
	return "/" + strings.Join(s, "/"), handle, true
}

func (g *Gateway) render(r *http.Request, route gatewayRoute) resp {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	return route.handle(g.app, r.WithContext(ctx))
}

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
	internal.Header.Del("Accept")
	route, ok := resolveGatewayRoute(internal)
	if !ok || route.category == "" || reservedAssetPath(u.Path) || !previewQuery(u.RawQuery, route.category == "posts" || route.category == "share") {
		return nil, gatewayRoute{}, false
	}
	return internal, route, true
}

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
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(20 * time.Second))
	// Safari <16.4 omits Sec-Fetch-Site.
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
	// nginx rate-limits previews per cf_clearance.
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
		if result.errorType == errorCodeRestricted {
			status = http.StatusUnavailableForLegalReasons
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

func (g *Gateway) recordMetric(route gatewayRoute, result resp, duration time.Duration) {
	category := cmp.Or(result.category, route.category)
	if category == "" || result.cacheHit {
		return
	}
	status := result.status
	if result.originStatus > 0 {
		status = result.originStatus
	}
	select {
	case g.metrics <- metricEvent{category: category, errorType: result.errorType, duration: duration, status: status}:
	default:
	}
}

func (g *Gateway) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, "GET", "HEAD") {
		return
	}
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
	g.writeResult(w, r, jsonResp(http.StatusOK, body))
}
