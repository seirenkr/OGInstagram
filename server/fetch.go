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
}

func statusOnly(status int, _ []byte) *AppError {
	if status != 200 {
		return igErr(status, errorCodeUpstream, http.StatusText(status))
	}
	return nil
}

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
	if spec.expectJSON {
		// API redirects lead to login/challenge HTML, not the requested media.
		// Keep the shared client's policy intact for other request types.
		noRedirect := *client
		noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
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
	if spec.expectJSON && ((status >= 300 && status < 400) || contentType == "text/html" || contentType == "application/xhtml+xml") {
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
	raw, readErr := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if ctx.Err() != nil {
		return status, "", contextAppError(ctx)
	}
	if readErr != nil {
		return status, "", requestError(readErr, spec.subject+" response read failed")
	}
	if len(raw) > maxResponseBytes {
		return status, "", igErr(502, errorCodeUpstream, spec.subject+" response too large")
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
	next, pending, skipped := 0, 0, 0
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
		if winner == "" || next-skipped > 1 || len(failures) > 0 {
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
			if r.err == errEmbedPaused {
				skipped++
			} else {
				failures = append(failures, r.name+": "+r.err.errorType()+": "+r.err.logMessage())
			}
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

func isInstagramURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "https" && (u.Hostname() == "www.instagram.com" || u.Hostname() == "i.instagram.com")
}

// One proxied request: pick a session, run it, then hold that session
// accountable. Ephemeral errors (deadlines, cooldowns) are ours, so only a
// non-ephemeral, rotatable code marks the session bad.
func (a *App) fetchViaProxy(ctx context.Context, spec fetchSpec) (status int, out string, ferr *AppError) {
	if ctx.Err() != nil {
		return 0, "", contextAppError(ctx)
	}
	s := a.pool.pick(strings.HasPrefix(spec.url, instagramOrigin+"/"))
	if s == nil {
		if len(a.pool.sessions) == 0 {
			return 0, "", igErr(503, errorCodeConnection, "Instagram proxy sessions are not configured")
		}
		return 0, "", ephemeralErr(503, errorCodeConnection, "all Instagram proxy sessions are cooling down")
	}

	started := time.Now()
	client := s.getClient()
	defer func() {
		elapsed := time.Since(started)
		// Instagram answers in well under a second. Running into a deadline of 3 s or
		// more means the exit IP hung (outlier ejection, as in Envoy): replace it.
		// A shorter caller deadline says nothing about the exit IP.
		hung := !spec.advisory && ferr != nil && ferr.Status == http.StatusGatewayTimeout && elapsed >= 3*time.Second
		rotated := hung || (!spec.advisory && ferr != nil && !ferr.Ephemeral && shouldRotate(ferr.Code))
		a.pool.done(s, client, elapsed, ferr == nil || hung)
		if rotated {
			rotated = a.pool.fail(s, client)
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

func (a *App) fetchOembed(ctx context.Context, shortcode string) oembedOutcome {
	status, body, err := a.fetchViaProxy(ctx, oembedSpec(shortcode))
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
	id, _, _ := strings.Cut(root.Get("media_id").String(), "_")
	return Post{
		Shortcode: shortcode,
		Username:  username,
		OwnerID:   root.Get("author_id").String(),
		FullName:  html.UnescapeString(firstGroup(oembedSharedByRE, root.Get("html").String())),
		Caption:   root.Get("title").String(),
		Attachments: []Attachment{{
			ID:        id,
			Kind:      "image",
			URL:       thumb,
			Thumbnail: thumb,
			Width:     width,
			Height:    height,
		}},
	}, true
}
