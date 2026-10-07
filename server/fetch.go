package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"html"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// One outbound request. interpret turns the upstream status and body into an
// AppError (nil accepts it); it runs inside fetchRequest so the deferred log
// still sees the final error.
type fetchSpec struct {
	operation string
	method    string
	url       string
	body      string
	subject   string
	headers   map[string]string
	interpret func(status int, raw []byte) *AppError
}

func statusOnly(status int, _ []byte) *AppError {
	if status != 200 {
		return igErr(status, errorCodeUpstream, http.StatusText(status))
	}
	return nil
}

func fetchRequest(ctx context.Context, client *http.Client, spec fetchSpec) (status int, out string, ferr *AppError) {
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
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", contextAppError(ctx)
		}
		return 0, "", requestError(err, spec.subject+" request failed")
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
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

func webLoggedOutSpec(shortcode string) fetchSpec {
	variables, _ := json.Marshal(map[string]any{
		"shortcode": shortcode,
		"__relay_internal__pv__PolarisAIGMMediaWebLabelEnabledrelayprovider": false,
	})
	lsd := rand.Text()
	form := url.Values{}
	form.Set("variables", string(variables))
	form.Set("doc_id", instagramWebLoggedOutDocID)
	form.Set("server_timestamps", "true")
	form.Set("lsd", lsd)
	return fetchSpec{
		operation: "post",
		subject:   "Instagram",
		interpret: instagramJSON,
		method:    http.MethodPost,
		url:       instagramOrigin + "/graphql/query",
		body:      form.Encode(),
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
			v, err := source.fetch(ctx)
			results <- result{name: source.name, value: v, persist: source.persist, err: err}
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
	report := func(winner string) {
		if winner == "" || next > 1 || len(failures) > 0 {
			logger(parent).InfoContext(parent, "staged fetch finished", "winner", winner, "started", next,
				"duration_ms", time.Since(started).Milliseconds(), "failures", failures)
		}
	}

	var lastErr *AppError
	for pending > 0 {
		select {
		case <-parent.Done():
			report("")
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
	if code := proxyBudgetErrorCode(err); code != "" {
		return budgetAppError(code)
	}
	return causedErr(502, errorCodeConnection, fallback, err)
}

func logOutbound(ctx context.Context, operation, session, method, rawURL string, started time.Time, status, bytes int, appErr *AppError, sessionRotated bool) {
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
		logger(ctx).InfoContext(ctx, "outbound request completed", append(attrs, "status", status, "bytes", bytes)...)
		return
	}
	attrs = append(attrs, "app_status", appErr.Status, "error_type", appErr.errorType(),
		"error", appErr.logMessage(), "session_rotated", sessionRotated)
	if status > 0 {
		attrs = append(attrs, "status", status)
	}
	logger(ctx).WarnContext(ctx, "outbound request failed", attrs...)
}

// One proxied request: pick a session, run it, then hold that session
// accountable. Ephemeral errors (deadline, budget) are ours, so only a
// non-ephemeral, rotatable code marks the session bad.
func (a *App) fetchViaProxy(ctx context.Context, spec fetchSpec) (status int, out string, ferr *AppError) {
	if ctx.Err() != nil {
		return 0, "", contextAppError(ctx)
	}
	s, pickReason := a.pool.pick(ctx)
	if s == nil {
		if pickReason == errorCodeBudgetBackend || pickReason == errorCodeBudgetExhausted {
			return 0, "", budgetAppError(pickReason)
		}
		if len(a.pool.sessions) == 0 {
			return 0, "", igErr(503, errorCodeConnection, "Instagram proxy sessions are not configured")
		}
		return 0, "", ephemeralErr(503, errorCodeConnection, "all Instagram proxy sessions are cooling down")
	}

	started := time.Now()
	defer func() {
		rotated := ferr != nil && !ferr.Ephemeral && shouldRotate(ferr.Code)
		logOutbound(ctx, spec.operation, s.name, spec.method, spec.url, started, status, len(out), ferr, rotated)
		switch {
		case ferr == nil:
			a.pool.recordLatency(s, time.Since(started))
		case rotated:
			a.pool.fail(s)
		case ferr.Status == http.StatusGatewayTimeout:
			// Our deadline, but the session did not answer in time: demote it.
			a.pool.recordLatency(s, requestTimeout)
		}
	}()

	return fetchRequest(ctx, s.getClient(), spec)
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
	if strings.HasPrefix(body, "<!DOCTYPE html") || strings.Contains(body, "require_login") {
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

type oembedOutcome struct {
	status int
	body   string
	ok     bool
}

func (a *App) fetchOembed(ctx context.Context, shortcode string) oembedOutcome {
	status, body, err := a.fetchViaProxy(ctx, fetchSpec{
		operation: "oembed", method: http.MethodGet, subject: "Instagram",
		url: instagramOrigin + "/api/v1/oembed/?" + url.Values{"url": {instagramOrigin + "/p/" + shortcode + "/"}}.Encode(),
		headers: map[string]string{
			"User-Agent":  instagramAppUA,
			"Accept":      "*/*",
			"X-IG-App-ID": instagramAppID,
		},
		// oEmbed's failure body carries Instagram's own reason, so any status is
		// worth reading; only transport failures are errors here.
		interpret: func(int, []byte) *AppError { return nil },
	})
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
		msg = errorCodeGeoBlocked
		baseErr.Code = errorCodeGeoBlocked
		baseErr.Status = 451
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
