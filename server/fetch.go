package main

import (
	"cmp"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/tidwall/gjson"
)

// One outbound request. interpret turns the upstream status and body into an
// AppError (nil accepts it); it runs inside fetchRequest so the deferred log
// still sees the final error.
type fetchSpec struct {
	operation  string
	method     string
	url        string
	body       string
	subject    string
	expectJSON bool
	headers    map[string]string
	interpret  func(status int, raw []byte) *AppError
	// An advisory request's failures say nothing about the exit IP: they never
	// rotate its session, and only its successes feed the RTT.
	advisory bool
	// cut keeps the first maxResponseBytes of a longer body instead of refusing it.
	cut      bool
	maxBytes int // 0: maxResponseBytes
	// dcRecheck asks for a successful IPv6 answer to be refetched through DC.
	dcRecheck func(body string) bool
	// page: follow redirects, except to the login wall.
	page bool
}

func statusOnly(status int, _ []byte) *AppError {
	if status != 200 {
		return igErr(status, errorCodeUpstream, http.StatusText(status))
	}
	return nil
}

const pageProbeBytes = 4 << 10

func fetchRequest(ctx context.Context, client *http.Client, spec fetchSpec) (status int, out string, ferr *AppError) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reader io.Reader
	if spec.body != "" {
		reader = strings.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, spec.url, reader)
	if err != nil {
		return 0, "", causedErr(500, "", "internal request error", err)
	}
	for k, v := range spec.headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "br, gzip")
	}
	if spec.expectJSON || spec.page {
		// API redirects lead to login/challenge HTML, not the requested media.
		// Keep the shared client's policy intact for other request types.
		noRedirect := *client
		noRedirect.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if !spec.page || loginWall(req.URL) {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		client = &noRedirect
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", contextAppError(ctx)
		}
		return 0, "", requestError(err, spec.subject+" request failed")
	}
	defer resp.Body.Close()
	wire := countingReader{resp.Body, outboundCounter(spec.operation)}
	status = resp.StatusCode
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	isPage := spec.expectJSON && (contentType == "text/html" || contentType == "application/xhtml+xml")
	pageError := func() (int, string, *AppError) {
		// Cancel before closing: HTTP/2 can stop the stream without draining it.
		cancel()
		if status >= 400 {
			if err := spec.interpret(status, nil); err != nil {
				return status, "", err
			}
		}
		code := errorCodeUpstream
		if isInstagramURL(spec.url) {
			code = errorCodeLoginRequired
		}
		return status, "", igErr(http.StatusBadGateway, code, spec.subject+" returned a page instead of JSON")
	}
	if spec.expectJSON && status >= 300 && status < 400 {
		return pageError()
	}
	readLimit := int64(cmp.Or(spec.maxBytes, maxResponseBytes))
	if isPage {
		// Some APIs label small JSON errors text/html; a few KB tells them from a real page.
		readLimit = pageProbeBytes
	}
	var body io.Reader = wire
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		decoded, err := gzip.NewReader(wire)
		if err != nil {
			if ctx.Err() != nil {
				return status, "", contextAppError(ctx)
			}
			return status, "", requestError(err, spec.subject+" response decompression failed")
		}
		defer decoded.Close()
		body = decoded
	case "br":
		body = brotli.NewReader(wire)
	default:
		return status, "", igErr(http.StatusBadGateway, errorCodeUpstream, spec.subject+" returned an unsupported encoding")
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, readLimit+1))
	if ctx.Err() != nil {
		return status, "", contextAppError(ctx)
	}
	if isPage && (readErr != nil || len(raw) > pageProbeBytes || !gjson.ValidBytes(raw)) {
		return pageError()
	}
	if readErr != nil {
		return status, "", requestError(readErr, spec.subject+" response read failed")
	}
	if int64(len(raw)) > readLimit {
		if !spec.cut {
			return status, "", igErr(502, errorCodeUpstream, spec.subject+" response too large")
		}
		raw = raw[:readLimit]
	}
	if err := spec.interpret(status, raw); err != nil {
		return status, "", err
	}
	return status, string(raw), nil
}

// Compressed body bytes per operation since the last "outbound bytes" line:
// proxy spend is billed on the wire, and sampled success lines cannot be summed.
var outboundBytes sync.Map // operation -> *atomic.Int64
var outboundBytesLoggedAt atomic.Int64

func outboundCounter(operation string) *atomic.Int64 {
	c, _ := outboundBytes.LoadOrStore(operation, new(atomic.Int64))
	return c.(*atomic.Int64)
}

type countingReader struct {
	io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func webLoggedOutSpec(shortcode string) fetchSpec {
	variables, _ := json.Marshal(map[string]any{
		"shortcode": shortcode,
		"__relay_internal__pv__PolarisAIGMMediaWebLabelEnabledrelayprovider": false,
	})
	lsd := rand.Text()
	form := url.Values{}
	form.Set("variables", string(variables))
	form.Set("doc_id", instagramWebLoggedOutDocID)
	form.Set("lsd", lsd)
	return fetchSpec{
		operation:  "post",
		subject:    "Instagram",
		expectJSON: true,
		interpret:  graphQLJSON,
		dcRecheck:  mutedMusic,
		method:     http.MethodPost,
		url:        instagramGraphQLOrigin + "/graphql/query",
		body:       form.Encode(),
		headers: map[string]string{
			"User-Agent":         "Mozilla/5.0",
			"Accept":             "*/*",
			"Content-Type":       "application/x-www-form-urlencoded",
			"X-FB-Friendly-Name": "PolarisPostRootQuery",
			"X-FB-LSD":           lsd,
		},
	}
}

type stagedSource[T any] struct {
	name    string // log label
	after   time.Duration
	persist bool
	fetch   func(context.Context) (T, *AppError)
}

// Sources race on a schedule; first valid result wins. A failure pulls the
// next source in early. Must be ordered by `after`, first at 0.
// The boolean is the winning source's persist (SQLite L2) policy; losing hedges do not affect it.
func stagedFetch[T any](parent context.Context, sources ...stagedSource[T]) (T, bool, *AppError) {
	type result struct {
		name    string
		value   T
		persist bool
		err     *AppError
	}
	var zero T
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan result, len(sources))

	started := time.Now()
	next, pending := 0, 0
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	start := func() {
		if next >= len(sources) || ctx.Err() != nil {
			return
		}
		source := sources[next]
		next++
		pending++
		go func() {
			r := result{name: source.name, persist: source.persist}
			defer func() {
				if p := recover(); p != nil {
					logger(parent).ErrorContext(parent, "staged source panicked", "source", source.name, "panic", p, "stack", string(debug.Stack()))
					r.err = ephemeralErr(http.StatusBadGateway, errorCodeUpstream, source.name+" failed")
				}
				results <- r
			}()
			r.value, r.err = source.fetch(ctx)
		}()
	}

	arm := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		timerC = nil
		if next >= len(sources) {
			return
		}
		delay := max(sources[next].after-time.Since(started), 0)
		timer = time.NewTimer(delay)
		timerC = timer.C
	}

	start()
	arm()

	// One line per fetch that hedged or failed: the hedge rate and parse
	// failures (format drift) are otherwise invisible. Clean first-source wins log nothing.
	var failures []string
	report := func(winner string, extra ...any) {
		if winner == "" || next > 1 || len(failures) > 0 {
			logger(parent).InfoContext(parent, "staged fetch finished", append([]any{"winner", winner, "started", next,
				"duration_ms", time.Since(started).Milliseconds(), "failures", failures}, extra...)...)
		}
	}

	var lastErr *AppError
	for pending > 0 {
		select {
		case <-parent.Done():
			report("", "cut", parent.Err().Error(), "pending", pending)
			if lastErr != nil && !isTransient(lastErr.Code) {
				return zero, false, lastErr
			}
			return zero, false, contextAppError(parent)
		case <-timerC:
			start()
			arm()
		case r := <-results:
			pending--
			if r.err == nil {
				report(r.name)
				return r.value, r.persist, nil
			}
			failures = append(failures, r.name+": "+r.err.errorType()+": "+r.err.logMessage())
			if r.err.Final {
				report("")
				return zero, false, r.err
			}
			lastErr = preferredError(lastErr, r.err)
			start()
			arm()
		}
	}
	report("")
	if lastErr == nil {
		lastErr = ephemeralErr(http.StatusBadGateway, errorCodeConnection, "upstream fetch failed")
	}
	return zero, false, lastErr
}

func requestError(err error, fallback string) *AppError {
	return causedErr(502, errorCodeConnection, fallback, err)
}

func logOutbound(ctx context.Context, operation, session, method, rawURL string, started time.Time, status, bytes int, appErr *AppError, sessionRotated bool) {
	if last := outboundBytesLoggedAt.Load(); time.Since(time.Unix(0, last)) >= time.Minute &&
		outboundBytesLoggedAt.CompareAndSwap(last, time.Now().UnixNano()) {
		var totals []any
		outboundBytes.Range(func(op, n any) bool {
			if v := n.(*atomic.Int64).Swap(0); v > 0 {
				totals = append(totals, op.(string), v)
			}
			return true
		})
		if len(totals) > 0 {
			slog.Info("outbound bytes", totals...)
		}
	}
	if (appErr == nil && mathrand.IntN(100) != 0) || (appErr != nil && appErr.Status == 499) {
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		u = &url.URL{}
	}
	attrs := []any{"operation", operation, "method", method, "host", u.Hostname(), "path", u.Path,
		"session", session, "duration_ms", time.Since(started).Milliseconds()}
	if appErr == nil {
		logger(ctx).InfoContext(ctx, "outbound request completed", append(attrs, "status", status, "bytes", bytes, "sample_rate", 100)...)
		return
	}
	attrs = append(attrs, "app_status", appErr.Status, "error_type", appErr.errorType(),
		"error", appErr.logMessage(), "session_rotated", sessionRotated)
	if status > 0 {
		attrs = append(attrs, "status", status)
	}
	logger(ctx).WarnContext(ctx, "outbound request failed", attrs...)
}

func loginWall(u *url.URL) bool {
	if !isInstagramURL(u.String()) {
		return false
	}
	for _, path := range []string{"/accounts/login", "/challenge", "/checkpoint"} {
		if u.Path == path || strings.HasPrefix(u.Path, path+"/") {
			return true
		}
	}
	return false
}

func isInstagramURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "https" && (u.Hostname() == "www.instagram.com" || u.Hostname() == "i.instagram.com")
}

// fetchViaProxy tries the IPv6 egress first and falls back to the DC proxy when
// the source is to blame; answers about the content return as is.
func (a *App) fetchViaProxy(ctx context.Context, spec fetchSpec) (int, string, *AppError) {
	v6 := a.v6
	if spec.page {
		v6 = a.v6Pages // a page's login wall must not rotate API sessions
	}
	if v6 != nil && isInstagramURL(spec.url) && mathrand.IntN(100) < a.cfg.EgressPercent {
		gate := a.v6Gate(spec.operation)
		if gate.allow() {
			// Leave DC at least half of the time left.
			budget := ipv6AttemptTimeout
			if spec.page {
				budget = ipv6PageAttemptTimeout
			}
			if deadline, ok := ctx.Deadline(); ok {
				budget = min(budget, time.Until(deadline)/2)
			}
			v6ctx, cancel := context.WithTimeout(ctx, budget)
			v6spec := spec
			v6spec.operation += "_v6" // count free IPv6 apart from paid DC
			status, out, err := a.fetchOn(v6ctx, v6, v6spec, budget)
			cancel()
			switch {
			case ctx.Err() != nil:
				gate.release()
				return status, out, err
			case err == nil:
				gate.ok(spec.operation)
				if spec.dcRecheck == nil || !spec.dcRecheck(out) {
					return status, out, nil
				}
				if dcStatus, dcOut, dcErr := a.fetchDC(ctx, spec, dcRecheckTimeout, proxyHungAfter); dcErr == nil && !spec.dcRecheck(dcOut) {
					return dcStatus, dcOut, nil
				}
				return status, out, nil
			case !err.Ephemeral && !shouldRotate(err.Code) && status < 500:
				gate.ok(spec.operation)
				return status, out, err
			default:
				gate.fail(spec.operation)
			}
		}
	}
	return a.fetchDC(ctx, spec, dcAttemptTimeout, dcAttemptTimeout)
}

// fetchDC bounds one DC request; an exit still waiting at hungAfter is replaced.
func (a *App) fetchDC(ctx context.Context, spec fetchSpec, limit, hungAfter time.Duration) (int, string, *AppError) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return a.fetchOn(ctx, a.pool, spec, hungAfter)
}

// One request on pool: pick a session, run it, then hold that session
// accountable. Ephemeral errors (deadlines, cooldowns) are ours, so only a
// non-ephemeral, rotatable code marks the session bad.
func (a *App) fetchOn(ctx context.Context, pool *SessionPool, spec fetchSpec, hungAfter time.Duration) (status int, out string, ferr *AppError) {
	if ctx.Err() != nil {
		return 0, "", contextAppError(ctx)
	}
	s := pool.pick(strings.HasPrefix(spec.url, instagramOrigin+"/"))
	if s == nil {
		if len(pool.sessions) == 0 {
			return 0, "", igErr(503, errorCodeConnection, "Instagram proxy sessions are not configured")
		}
		return 0, "", ephemeralErr(503, errorCodeConnection, "all Instagram proxy sessions are cooling down")
	}

	started := time.Now()
	client := s.getClient()
	defer func() {
		elapsed := time.Since(started)
		// Instagram answers in well under a second. Running into a deadline of
		// hungAfter or more means the exit IP hung (outlier ejection, as in Envoy):
		// replace it. A shorter caller deadline says nothing about the exit IP.
		hung := !spec.advisory && ferr != nil && ferr.Status == http.StatusGatewayTimeout && elapsed >= hungAfter
		rotated := hung || (!spec.advisory && ferr != nil && !ferr.Ephemeral && shouldRotate(ferr.Code))
		pool.done(s, client, elapsed, ferr == nil || hung)
		if rotated {
			rotated = pool.fail(s, client)
		}
		logOutbound(ctx, spec.operation, s.name, spec.method, spec.url, started, status, len(out), ferr, rotated)
	}()

	return fetchRequest(ctx, client, spec)
}

func instagramJSON(status int, raw []byte) *AppError {
	body := string(raw)
	if status >= 400 {
		msg := http.StatusText(status)
		if gjson.Valid(body) {
			msg = cmp.Or(strings.TrimSpace(gjson.Get(body, "message").String()), msg)
		}
		code := errorCodeUpstream
		switch status {
		case 400:
			code = errorCodeBadRequest
		case 401:
			code = errorCodeUnauthorized
		case 403:
			code = errorCodeForbidden
		case 404:
			code = errorCodeNotFound
		case 429:
			code = errorCodeRateLimited
		}
		return causedErr(status, code, "Instagram request failed", errors.New(msg))
	}
	if strings.HasPrefix(body, "<!DOCTYPE html") || gjson.Get(body, "require_login").Bool() {
		return igErr(401, errorCodeLoginRequired, "Instagram requires login to view this content")
	}
	if !gjson.Valid(body) {
		return igErr(502, errorCodeJSONDecode, "Instagram returned an unreadable response")
	}
	parsed := gjson.Parse(body)
	if parsed.Get("status").String() == "fail" {
		msg := cmp.Or(strings.TrimSpace(parsed.Get("message").String()), "Instagram request failed")
		return causedErr(502, errorCodeGraphQL, "Instagram request failed", errors.New(msg))
	}
	return nil
}

// GraphQL signals throttling in a 200 body, and its other HTTP errors come from
// the endpoint (retired doc_id, rejected lsd), not from the post or the exit IP.
func graphQLJSON(status int, raw []byte) *AppError {
	if err := instagramJSON(status, raw); err != nil {
		if status >= 400 && status != 401 && status != 403 && status != 429 {
			err.Code = errorCodeGraphQL
		}
		return err
	}
	first := gjson.GetBytes(raw, "errors.0")
	if code, msg := first.Get("code").Int(), first.Get("message").String(); code == graphQLRateLimitedCode || strings.Contains(msg, "Please wait") {
		return causedErr(429, errorCodeRateLimited, "Instagram rate limited the request", fmt.Errorf("graphql %d: %s", code, msg))
	}
	return nil
}

// mutedMusic: Instagram mutes licensed music in the VPS's region, not on DC's US exits.
func mutedMusic(body string) bool {
	item := gjson.Get(body, "data.xdt_api__v1__media__shortcode__web_info.items.0")
	if item.Get("clips_metadata.music_info.music_consumption_info.should_mute_audio").Bool() {
		return true
	}
	muted := false
	for _, m := range append([]gjson.Result{item}, item.Get("carousel_media").Array()...) {
		muted = muted || m.Get("media_type").Int() == 2 && m.Get("has_audio").Type == gjson.False
	}
	return muted
}

func pageSpec(operation, rawURL string) fetchSpec {
	return fetchSpec{
		operation: operation, method: http.MethodGet, url: rawURL, subject: "Instagram", page: true,
		headers: map[string]string{"User-Agent": embedUA},
		interpret: func(status int, raw []byte) *AppError {
			switch {
			case status >= 300 && status < 400:
				return igErr(http.StatusBadGateway, errorCodeLoginRequired, "Instagram redirected to login or verification")
			case status == http.StatusNotFound:
				return igErr(http.StatusNotFound, errorCodeNotFound, "Instagram has no such page")
			}
			return statusOnly(status, raw)
		},
	}
}

// loggedOutQuerySpec posts a logged-out web app query; the trailing slash matters.
func loggedOutQuerySpec(operation, friendlyName, docID string, variables any) fetchSpec {
	encoded, _ := json.Marshal(variables)
	lsd := rand.Text()
	return fetchSpec{
		operation: operation, method: http.MethodPost, subject: "Instagram", expectJSON: true, interpret: graphQLJSON,
		url:  instagramGraphQLOrigin + "/api/graphql/",
		body: url.Values{"variables": {string(encoded)}, "doc_id": {docID}, "lsd": {lsd}}.Encode(),
		headers: map[string]string{
			"User-Agent":         "Mozilla/5.0",
			"Accept":             "*/*",
			"Content-Type":       "application/x-www-form-urlencoded",
			"X-FB-Friendly-Name": friendlyName,
			"X-FB-LSD":           lsd,
		},
	}
}

// v6Gate pauses IPv6 for one operation after ipv6Strikes failures in the last
// ipv6Window: Instagram also limits a whole /64 per endpoint.
type v6Gate struct {
	mu       sync.Mutex
	recent   [ipv6Window]bool // true: failed
	next     int
	failures int
	until    time.Time
	pause    time.Duration
	probeAt  time.Time
}

func (a *App) v6Gate(operation string) *v6Gate {
	g, _ := a.v6Gates.LoadOrStore(operation, &v6Gate{})
	return g.(*v6Gate)
}

func (g *v6Gate) allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.until.IsZero() {
		return true
	}
	if now.Before(g.until) || now.Sub(g.probeAt) < ipv6PageAttemptTimeout*2 {
		return false
	}
	g.probeAt = now
	return true
}

// release frees a probe that ended without an answer.
func (g *v6Gate) release() {
	g.mu.Lock()
	g.probeAt = time.Time{}
	g.mu.Unlock()
}

func (g *v6Gate) record(failed bool) {
	if g.recent[g.next] {
		g.failures--
	}
	g.recent[g.next] = failed
	if failed {
		g.failures++
	}
	g.next = (g.next + 1) % ipv6Window
}

func (g *v6Gate) ok(operation string) {
	g.mu.Lock()
	resumed := !g.until.IsZero()
	if resumed {
		*g = v6Gate{}
	} else {
		g.record(false)
	}
	g.mu.Unlock()
	if resumed {
		slog.Info("ipv6 egress resumed", "operation", operation)
	}
}

func (g *v6Gate) fail(operation string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.until) {
		return // a straggler from before the pause
	}
	g.record(true)
	if g.until.IsZero() && g.failures < ipv6Strikes {
		return
	}
	g.pause = min(max(g.pause*2, ipv6PauseMin), ipv6PauseMax)
	g.until, g.probeAt = time.Now().Add(g.pause), time.Time{}
	slog.Warn("ipv6 egress paused", "operation", operation, "pause_s", int(g.pause.Seconds()))
}

type oembedOutcome struct {
	status int
	body   string
	ok     bool
}

func oembedSpec(shortcode string) fetchSpec {
	return fetchSpec{
		operation: "oembed", method: http.MethodGet, subject: "Instagram",
		expectJSON: true,
		url:        instagramOrigin + "/api/v1/oembed/?" + url.Values{"url": {instagramOrigin + "/p/" + shortcode + "/"}}.Encode(),
		headers: map[string]string{
			"User-Agent":  instagramAppUA,
			"Accept":      "*/*",
			"X-IG-App-ID": instagramAppID,
		},
		// oEmbed's JSON failure body carries Instagram's own reason for
		// oembedVerdict; HTML pages, throttling and server errors are classified here.
		interpret: func(status int, raw []byte) *AppError {
			if raw == nil || status == http.StatusTooManyRequests || status >= 500 {
				return instagramJSON(status, raw)
			}
			return nil
		},
	}
}

// oEmbed's gate verdicts depend on location, so it stays on DC's US exits.
func (a *App) fetchOembed(ctx context.Context, shortcode string) oembedOutcome {
	status, body, err := a.fetchOn(ctx, a.pool, oembedSpec(shortcode), proxyHungAfter)
	return oembedOutcome{status: status, body: body, ok: err == nil}
}

func oembedVerdict(o oembedOutcome, shortcode string, baseErr AppError) (Post, *AppError) {
	if !o.ok || !gjson.Valid(o.body) {
		return Post{}, &baseErr
	}
	if o.status >= 200 && o.status < 300 {
		if post, parsed := parseOembedPost(shortcode, o.body); parsed {
			return post, nil
		}
	}
	root := gjson.Parse(o.body)
	if root.Get("status").String() != "fail" {
		return Post{}, &baseErr
	}
	msg := root.Get("message").String()
	if msg == "geoblock_required" {
		// Every gate kind gets this message; only gating type 2 is regional. Either
		// way it is Instagram's verdict on the post, so it caches like one.
		baseErr.Code, baseErr.Status = errorCodeRestricted, http.StatusForbidden
		if root.Get("gating_type_thrift").Int() == 2 {
			baseErr.Code, baseErr.Status = errorCodeGeoBlocked, http.StatusUnavailableForLegalReasons
		}
		msg = baseErr.Code
		baseErr.Ephemeral, baseErr.Final = false, false
	}
	if title := root.Get("title").String(); msg != "" && title != "" {
		baseErr.CardCode, baseErr.CardTitle, baseErr.CardDesc = msg, title, root.Get("description").String()
	}
	return Post{}, &baseErr
}

var oembedSharedByRE = regexp.MustCompile(`A post shared by (.*?) \(@`)

func parseOembedPost(shortcode, body string) (Post, bool) {
	root := gjson.Parse(body)
	username := root.Get("author_name").String()
	thumb := root.Get("thumbnail_url").String()
	if !validShortcode(shortcode) || !validUsername(username) || !validCachedURL(thumb) {
		return Post{}, false
	}
	width, height := uintOf(root, "thumbnail_width"), uintOf(root, "thumbnail_height")
	if !validCachedDimensions(width, height) {
		return Post{}, false
	}
	return Post{
		Shortcode: shortcode,
		Partial:   true, // a thumbnail only
		Username:  username,
		FullName:  html.UnescapeString(firstGroup(oembedSharedByRE, root.Get("html").String())),
		Caption:   root.Get("title").String(),
		Attachments: []Attachment{{
			Kind:      "image",
			URL:       thumb,
			Thumbnail: thumb,
			Width:     width,
			Height:    height,
		}},
	}, true
}
