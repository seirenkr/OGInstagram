package main

import (
	"bytes"
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

type fetchSpec struct {
	operation       string
	method          string
	url             string
	body            string
	subject         string
	expectJSON      bool
	headers         map[string]string
	interpret       func(status int, raw []byte) *AppError
	advisory        bool
	maxBytes        int
	dcRecheck       func(body string) bool
	recheckSoftFail bool
	ipv6Budget      time.Duration
	dcTimeout       time.Duration
	page            bool
	redirect        bool
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
	if spec.expectJSON || spec.page || spec.redirect {
		noRedirect := *client
		noRedirect.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if spec.redirect {
				if len(via) < 3 && isShareRedirectHop(via[len(via)-1].URL, req.URL) {
					return nil
				}
				return http.ErrUseLastResponse
			}
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
	status = resp.StatusCode
	if spec.redirect {
		location := ""
		if status >= 300 && status < 400 {
			if rawLoc := strings.TrimSpace(resp.Header.Get("Location")); rawLoc != "" {
				if resolved, err := resp.Request.URL.Parse(rawLoc); err == nil {
					location = resolved.String()
				}
			}
		}
		if err := spec.interpret(status, []byte(location)); err != nil {
			return status, "", err
		}
		return status, location, nil
	}
	wire := countingReader{resp.Body, outboundCounter(spec.operation)}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	isPage := spec.expectJSON && (contentType == "text/html" || contentType == "application/xhtml+xml")
	pageError := func() (int, string, *AppError) {
		// Cancel before closing so HTTP/2 resets the stream without draining it.
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
		// Some APIs label small JSON errors or oEmbed's "No Media Match" 404 as text/html.
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
	plainOembedMiss := spec.operation == "oembed" && status == http.StatusNotFound && bytes.Equal(bytes.TrimSpace(raw), []byte("No Media Match"))
	if isPage && (readErr != nil || len(raw) > pageProbeBytes || (!gjson.ValidBytes(raw) && !plainOembedMiss)) {
		return pageError()
	}
	if readErr != nil {
		return status, "", requestError(readErr, spec.subject+" response read failed")
	}
	if int64(len(raw)) > readLimit {
		if spec.maxBytes == 0 {
			return status, "", igErr(502, errorCodeUpstream, spec.subject+" response too large")
		}
		raw = raw[:readLimit]
	}
	if err := spec.interpret(status, raw); err != nil {
		return status, "", err
	}
	return status, string(raw), nil
}

var outboundBytes sync.Map
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
	var pk string
	if n := shortcodePK(shortcode); n != nil {
		pk = n.String()
	}
	spec := loggedOutQuerySpec("post", "PolarisLoggedOutDesktopWWWPostRootContentQuery", instagramWebLoggedOutDocID, map[string]any{"media_id": pk})
	spec.dcRecheck = mutedMusic
	return spec
}

type stagedSource[T any] struct {
	name  string
	after time.Duration
	fetch func(context.Context) (T, *AppError)
}

func stagedFetch[T any](parent context.Context, sources ...stagedSource[T]) (T, *AppError) {
	type result struct {
		name  string
		value T
		err   *AppError
	}
	var zero T
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan result, len(sources))

	started := time.Now()
	next, pending := 0, 0
	var timerC <-chan time.Time

	start := func() {
		if next >= len(sources) || ctx.Err() != nil {
			return
		}
		source := sources[next]
		next++
		pending++
		go func() {
			r := result{name: source.name}
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
		timerC = nil
		if next < len(sources) {
			timerC = time.After(max(sources[next].after-time.Since(started), 0))
		}
	}

	start()
	arm()

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
				return zero, lastErr
			}
			return zero, contextAppError(parent)
		case <-timerC:
			start()
			arm()
		case r := <-results:
			pending--
			if r.err == nil {
				report(r.name)
				return r.value, nil
			}
			failures = append(failures, r.name+": "+r.err.errorType()+": "+r.err.logMessage())
			if r.err.Final {
				report("")
				return zero, r.err
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
	return zero, lastErr
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

func isLoginWallPath(path string) bool {
	for _, prefix := range []string{"/accounts/login", "/accounts/onetap", "/challenge", "/checkpoint"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func loginWall(u *url.URL) bool {
	return isInstagramURL(u.String()) && isLoginWallPath(u.Path)
}

func isInstagramURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "https" && (u.Hostname() == "www.instagram.com" || u.Hostname() == "i.instagram.com")
}

func (a *App) fetchViaProxy(ctx context.Context, spec fetchSpec) (int, string, *AppError) {
	status, out, _, err := a.fetchViaProxySource(ctx, spec)
	return status, out, err
}

func (a *App) fetchViaProxySource(ctx context.Context, spec fetchSpec) (int, string, bool, *AppError) {
	v6 := a.v6
	if spec.page || spec.redirect {
		v6 = a.v6Pages
	}
	if v6 != nil && isInstagramURL(spec.url) && mathrand.IntN(100) < a.cfg.EgressPercent {
		baseBudget := spec.ipv6Budget
		if baseBudget == 0 {
			baseBudget = ipv6AttemptTimeout
			if spec.page || spec.redirect {
				baseBudget = ipv6PageAttemptTimeout
			}
		}
		budget := baseBudget
		if deadline, ok := ctx.Deadline(); ok {
			budget = min(budget, time.Until(deadline)/2)
		}
		gate := a.v6Gate(spec.operation)
		if budget >= ipv6MinBudget && gate.allow() {
			started := time.Now()
			v6Deadline := started.Add(budget)
			v6spec := spec
			v6spec.operation += "_v6"
			hungAfter := min(baseBudget, ipv6HungAfter)
			status, out, err := a.fetchOn(ctx, v6, v6spec, budget, hungAfter)
			for retry := 0; retry < ipv6Retries && err != nil && err.Code == errorCodeRateLimited && ctx.Err() == nil; retry++ {
				remaining := time.Until(v6Deadline)
				if remaining < ipv6MinBudget {
					break
				}
				status, out, err = a.fetchOn(ctx, v6, v6spec, remaining, hungAfter)
			}
			switch {
			case ctx.Err() != nil:
				gate.release(started)
				return status, out, true, err
			case err == nil:
				if spec.dcRecheck == nil || !spec.dcRecheck(out) {
					gate.ok(spec.operation, started)
					return status, out, true, nil
				}
				dcLimit := cmp.Or(spec.dcTimeout, dcRecheckTimeout)
				dcStatus, dcOut, dcErr := a.fetchDC(ctx, spec, dcLimit, proxyHungAfter)
				if dcErr != nil {
					if spec.recheckSoftFail {
						gate.release(started)
						return dcStatus, "", false, dcErr
					}
					gate.ok(spec.operation, started)
					return status, out, true, nil
				}
				if !spec.dcRecheck(dcOut) {
					if spec.recheckSoftFail {
						gate.fail(spec.operation)
					} else {
						gate.ok(spec.operation, started)
					}
					return dcStatus, dcOut, false, nil
				}
				gate.ok(spec.operation, started)
				return dcStatus, dcOut, false, nil
			case err.Ephemeral && (err.Status == http.StatusServiceUnavailable || (err.Status == http.StatusGatewayTimeout && budget < baseBudget)):
				gate.release(started)
			case !err.Ephemeral && !shouldRotate(err.Code) && status < 500:
				gate.ok(spec.operation, started)
				return status, out, true, err
			default:
				gate.fail(spec.operation)
			}
		}
	}
	dcLimit := cmp.Or(spec.dcTimeout, dcAttemptTimeout)
	status, out, err := a.fetchDC(ctx, spec, dcLimit, dcLimit)
	return status, out, false, err
}

func (a *App) fetchDC(ctx context.Context, spec fetchSpec, limit, hungAfter time.Duration) (int, string, *AppError) {
	if spec.advisory {
		return a.fetchOn(ctx, a.pool, spec, limit, hungAfter)
	}
	gate := a.dcGate(spec.operation)
	if !gate.allow() {
		return 0, "", ephemeralErr(http.StatusServiceUnavailable, errorCodeConnection, "Instagram keeps refusing "+spec.operation+"; paused")
	}
	started := time.Now()
	status, out, err := a.fetchOn(ctx, a.pool, spec, limit, hungAfter)
	switch {
	case err != nil && err.Ephemeral:
		gate.release(started)
	case err != nil && shouldRotate(err.Code):
		gate.fail(spec.operation)
	default:
		gate.ok(spec.operation, started)
	}
	return status, out, err
}

func (a *App) fetchOn(ctx context.Context, pool *SessionPool, spec fetchSpec, attemptTimeout, hungAfter time.Duration) (status int, out string, ferr *AppError) {
	if ctx.Err() != nil {
		return 0, "", contextAppError(ctx)
	}
	s := pool.pick()
	if s == nil {
		if len(pool.sessions) == 0 {
			return 0, "", igErr(503, errorCodeConnection, "Instagram proxy sessions are not configured")
		}
		return 0, "", ephemeralErr(503, errorCodeConnection, "all Instagram proxy sessions are cooling down")
	}

	started := time.Now()
	reqCtx := ctx
	if attemptTimeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
		defer cancel()
	}
	client := s.getClient()
	defer func() {
		elapsed := time.Since(started)
		attemptTimedOut := ctx.Err() == nil && reqCtx.Err() == context.DeadlineExceeded
		hung := !spec.advisory && ferr != nil && ferr.Status == http.StatusGatewayTimeout &&
			(elapsed >= hungAfter || (attemptTimedOut && attemptTimeout >= hungAfter))
		rotated := hung || (!spec.advisory && ferr != nil && !ferr.Ephemeral && shouldRotate(ferr.Code))
		pool.done(s, client, elapsed, ferr == nil || hung)
		if rotated {
			rotated = pool.fail(s, client)
		}
		loggedBytes := len(out)
		if spec.redirect {
			loggedBytes = 0
		}
		logOutbound(ctx, spec.operation, s.name, spec.method, spec.url, started, status, loggedBytes, ferr, rotated)
	}()

	return fetchRequest(reqCtx, client, spec)
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

// Instagram mutes licensed audio in the VPS's region, not on DC's US exits.
func mutedMusic(body string) bool {
	item := gjson.Get(body, loggedOutItemPath)
	reasonType := item.Get("clips_metadata.original_sound_info.consumption_info.should_mute_audio_reason_type")
	hasMuteReasonType := present(reasonType) && reasonType.Type != gjson.False && reasonType.String() != "" && reasonType.String() != "0"
	if item.Get("clips_metadata.music_info.music_consumption_info.should_mute_audio").Bool() ||
		item.Get("clips_metadata.original_sound_info.should_mute_audio").Bool() ||
		item.Get("clips_metadata.original_sound_info.consumption_info.should_mute_audio_reason").String() != "" ||
		hasMuteReasonType {
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

// Trailing slash on /api/graphql/ is required.
func loggedOutQuerySpec(operation, friendlyName, docID string, variables any) fetchSpec {
	encoded, _ := json.Marshal(variables)
	lsd := rand.Text()
	return fetchSpec{
		operation: operation, method: http.MethodPost, subject: "Instagram", expectJSON: true, interpret: graphQLJSON,
		url: instagramOrigin + "/api/graphql/",
		body: url.Values{
			"variables": {string(encoded)}, "doc_id": {docID}, "lsd": {lsd},
			"__a": {"1"}, "__comet_req": {"7"},
			"__hblp": {haveAllResources}, "__hsdp": {haveAllResources}, "__sjsp": {haveAllResources}, "__csr": {haveAllResources}, "__dyn": {haveAllResources},
		}.Encode(),
		headers: map[string]string{
			"User-Agent":         "Mozilla/5.0",
			"Accept":             "*/*",
			"Content-Type":       "application/x-www-form-urlencoded",
			"X-FB-Friendly-Name": friendlyName,
			"X-FB-LSD":           lsd,
		},
	}
}

type v6Gate struct {
	strikes      int
	probeTimeout time.Duration
	source       string
	mu           sync.Mutex
	recent       [ipv6Window]bool
	next         int
	failures     int
	streak       int
	until        time.Time
	pause        time.Duration
	probeAt      time.Time
}

func (a *App) v6Gate(operation string) *v6Gate {
	g, _ := a.v6Gates.LoadOrStore(operation, &v6Gate{})
	return g.(*v6Gate)
}

func (a *App) dcGate(operation string) *v6Gate {
	g, _ := a.v6Gates.LoadOrStore("dc "+operation, &v6Gate{strikes: ipv6Window, probeTimeout: dcAttemptTimeout * 2, source: "dc proxy"})
	return g.(*v6Gate)
}

func (g *v6Gate) allow() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.until.IsZero() {
		return true
	}
	if now.Before(g.until) || now.Sub(g.probeAt) < cmp.Or(g.probeTimeout, ipv6AttemptTimeout*2) {
		return false
	}
	g.probeAt = now
	return true
}

func (g *v6Gate) release(started time.Time) {
	g.mu.Lock()
	if !g.until.IsZero() && !started.Before(g.until) {
		g.probeAt = time.Time{}
	}
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

func (g *v6Gate) ok(operation string, started time.Time) {
	g.mu.Lock()
	resumed := !g.until.IsZero() && !started.Before(g.until)
	switch {
	case resumed:
		g.recent, g.next, g.failures, g.until, g.probeAt = [ipv6Window]bool{}, 0, 0, time.Time{}, time.Time{}
		g.pause /= 2
		g.streak = 1
	case g.until.IsZero():
		g.record(false)
		g.streak++
		if g.pause > 0 && g.streak >= ipv6Window {
			g.pause = 0
		}
	}
	g.mu.Unlock()
	if resumed {
		slog.Info(cmp.Or(g.source, "ipv6 egress")+" resumed", "operation", operation)
	}
}

func (g *v6Gate) fail(operation string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.until) {
		return
	}
	g.streak = 0
	g.record(true)
	if g.until.IsZero() && g.failures < cmp.Or(g.strikes, ipv6Strikes) {
		return
	}
	g.pause = min(max(g.pause*2, ipv6PauseMin), ipv6PauseMax)
	g.until, g.probeAt = time.Now().Add(g.pause), time.Time{}
	slog.Warn(cmp.Or(g.source, "ipv6 egress")+" paused", "operation", operation, "pause_s", int(g.pause.Seconds()))
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
	status, body, err := a.fetchOn(ctx, a.pool, oembedSpec(shortcode), 0, proxyHungAfter)
	return oembedOutcome{status: status, body: body, ok: err == nil}
}

func oembedNotFound(o oembedOutcome) bool {
	return o.ok && o.status == http.StatusNotFound &&
		(strings.TrimSpace(o.body) == "No Media Match" || gjson.Valid(o.body))
}

func gatingError(gatingType int, title, desc string, base AppError) *AppError {
	base.Code, base.Status = errorCodeRestricted, http.StatusForbidden
	if gatingType == 2 {
		base.Code, base.Status = errorCodeGeoBlocked, http.StatusUnavailableForLegalReasons
	}
	base.Ephemeral, base.Final = false, false
	if title != "" {
		base.CardCode, base.CardTitle, base.CardDesc = base.Code, title, desc
	}
	return &base
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
		return Post{}, gatingError(int(root.Get("gating_type_thrift").Int()), root.Get("title").String(), root.Get("description").String(), baseErr)
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
		Partial:   true,
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
