package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

type gqlSpec struct {
	name    string
	target  string
	method  string
	url     string
	body    string
	headers map[string]string
}

func webLoggedOutSpec(shortcode string) gqlSpec {
	variables, _ := json.Marshal(map[string]any{
		"shortcode": shortcode,
		"__relay_internal__pv__PolarisAIGMMediaWebLabelEnabledrelayprovider": false,
	})
	lsd := newLSD()
	form := url.Values{}
	form.Set("variables", string(variables))
	form.Set("doc_id", instagramWebLoggedOutDocID)
	form.Set("server_timestamps", "true")
	form.Set("lsd", lsd)
	return gqlSpec{
		name:   "post",
		target: shortcode,
		method: http.MethodPost,
		url:    instagramOrigin + "/graphql/query",
		body:   form.Encode(),
		headers: map[string]string{
			// A modern-browser UA makes IG return an HTML login shell instead of
			// JSON here; a minimal UA gets the anonymous JSON payload.
			"User-Agent":         "Mozilla/5.0",
			"Accept":             "*/*",
			"Content-Type":       "application/x-www-form-urlencoded",
			"X-FB-Friendly-Name": "PolarisPostRootQuery",
			"X-FB-LSD":           lsd,
		},
	}
}

func (a *App) raceFetch(spec gqlSpec) (string, *AppError) {
	sessions := a.pool.pick(fetchRaceCount, nil)
	if len(sessions) == 0 {
		if a.pool.overBudget() {
			return "", ephemeralErr(503, reasonBudgetExceeded, "hourly request budget reached")
		}
		if len(a.pool.sessions) == 0 {
			return "", igErr(503, reasonConnection, "Instagram proxy sessions are not configured")
		}
		return "", ephemeralErr(503, reasonConnection, "all Instagram proxy sessions are rate limited or cooling down")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	racing := map[*Session]bool{}
	attempts := func(picked []*Session) []attempt[string] {
		out := make([]attempt[string], 0, len(picked))
		for _, s := range picked {
			racing[s] = true
			out = append(out, func() (string, *AppError) { return a.attemptFetch(ctx, spec, s) })
		}
		return out
	}
	return hedgedRace(attempts(sessions), func() []attempt[string] {
		return attempts(a.pool.pick(fetchHedgeCount, racing))
	})
}

type attempt[T any] func() (T, *AppError)

// hedgedRace runs the initial attempts concurrently and launches the hedge
// attempts once — when fetchHedgeDelay elapses or every launched attempt has
// failed — returning the first success. On total failure it returns the most
// permanent error seen (a real 404 beats a throttled IP's 401).
func hedgedRace[T any](initial []attempt[T], hedge func() []attempt[T]) (T, *AppError) {
	type result struct {
		value T
		err   *AppError
	}
	results := make(chan result)
	done := make(chan struct{})
	defer close(done)

	pending := 0
	launch := func(fs []attempt[T]) {
		pending += len(fs)
		for _, f := range fs {
			go func() {
				v, err := f()
				select {
				case results <- result{v, err}:
				case <-done:
				}
			}()
		}
	}
	launch(initial)

	hedged := false
	tryHedge := func() {
		if !hedged {
			hedged = true
			launch(hedge())
		}
	}

	timer := time.NewTimer(fetchHedgeDelay)
	defer timer.Stop()

	var lastErr *AppError
	for pending > 0 {
		select {
		case <-timer.C:
			tryHedge()
		case r := <-results:
			pending--
			if r.err == nil {
				return r.value, nil
			}
			if lastErr == nil || (isTransient(lastErr.Reason) && !isTransient(r.err.Reason)) {
				lastErr = r.err
			}
			if pending == 0 {
				tryHedge()
			}
		}
	}
	var zero T
	if lastErr == nil {
		lastErr = igErr(502, reasonClientError, "Instagram fetch failed")
	}
	return zero, lastErr
}

// logOutbound writes one access-log style line per outbound request, e.g.
// "POST https://www.instagram.com/graphql/query 200 812ms", so the Workers
// Logs list is scannable without expanding entries; details stay as fields.
func logOutbound(op, target, session, method, rawURL string, started time.Time, status, bytes int, ferr *AppError) {
	endpoint := rawURL
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		endpoint = endpoint[:i]
	}
	ms := time.Since(started).Milliseconds()
	msg := method + " " + endpoint + " " + strconv.Itoa(status) + " " + strconv.FormatInt(ms, 10) + "ms"
	if ferr == nil {
		slog.Info(msg, "op", op, "target", target, "status", status, "session", session, "ms", ms, "bytes", bytes)
		return
	}
	if ferr.Status == 499 { // cancelled race loser; not a real outcome
		return
	}
	if ferr.Reason != "" {
		msg += " reason=" + ferr.Reason
	}
	slog.Warn(msg, "op", op, "target", target, "status", status,
		"reason", ferr.Reason, "session", session, "ms", ms, "detail", ferr.Message)
}

func (a *App) attemptFetch(ctx context.Context, spec gqlSpec, s *Session) (body string, ferr *AppError) {
	started := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	defer func() {
		status := 200
		if ferr != nil {
			status = ferr.Status
		}
		logOutbound(spec.name, spec.target, s.name, spec.method, spec.url, started, status, len(body), ferr)
	}()

	var bodyReader io.Reader
	if spec.body != "" {
		bodyReader = strings.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(reqCtx, spec.method, spec.url, bodyReader)
	if err != nil {
		return "", igErr(500, "", err.Error())
	}
	for k, v := range spec.headers {
		req.Header.Set(k, v)
	}

	failed := false
	failOnce := func(reason string) {
		if !failed {
			failed = true
			a.pool.fail(s, reason)
		}
	}

	resp, err := s.getClient().Do(req)
	if err != nil {

		if ctx.Err() != nil {
			return "", igErr(499, "", "cancelled")
		}
		failOnce(reasonConnection)
		return "", igErr(502, reasonConnection, err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	bodyText := string(raw)

	if resp.StatusCode >= 400 {
		msg := instagramMessage(bodyText)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		reason := reasonClientError
		switch resp.StatusCode {
		case 401:
			reason = reasonUnauthorized
		case 403:
			reason = reasonForbidden
		case 400:
			reason = reasonBadRequest
		case 429:
			reason = reasonThrottled
		case 404:
			reason = reasonNotFound
		}

		if shouldRotate(reason) {
			failOnce(reason)
		}
		return "", igErr(resp.StatusCode, reason, msg)
	}

	if strings.HasPrefix(bodyText, "<!DOCTYPE html") || strings.Contains(bodyText, "require_login") {
		failOnce(reasonLoginRequired)
		return "", igErr(401, reasonLoginRequired, "Instagram requires login to view this content")
	}

	if !gjson.Valid(bodyText) {
		failOnce(reasonJSONDecode)
		return "", igErr(502, reasonJSONDecode, "Instagram returned an unreadable response")
	}

	parsed := gjson.Parse(bodyText)
	if parsed.Get("status").String() == "fail" {
		if shouldRotate(reasonGraphql) {
			failOnce(reasonGraphql)
		}
		msg := strings.TrimSpace(parsed.Get("message").String())
		if msg == "" {
			msg = "Instagram request failed"
		}
		return "", igErr(502, reasonGraphql, msg)
	}

	a.pool.recordLatency(s, time.Since(started))
	a.pool.countRequest()
	return bodyText, nil
}

func (a *App) proxyRawGet(op, target, rawURL string, headers map[string]string) (int, string, bool) {
	sessions := a.pool.pick(1, nil)
	if len(sessions) == 0 {
		return 0, "", false
	}
	s := sessions[0]
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.getClient().Do(req)
	if err != nil {
		logOutbound(op, target, s.name, http.MethodGet, rawURL, started, 502, 0, igErr(502, reasonConnection, err.Error()))
		return 0, "", false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	logOutbound(op, target, s.name, http.MethodGet, rawURL, started, resp.StatusCode, len(raw), nil)
	return resp.StatusCode, string(raw), true
}

func instagramMessage(body string) string {
	if !gjson.Valid(body) {
		return ""
	}
	return strings.TrimSpace(gjson.Get(body, "message").String())
}

func (c Config) oembedURL(shortcode string) string {
	q := url.Values{}
	q.Set("url", instagramOrigin+"/p/"+shortcode+"/")
	return instagramOrigin + "/api/v1/oembed/?" + q.Encode()
}

// oembedFallback queries the public oembed endpoint after both primary
// fetches have failed; some posts still resolve there. A success payload
// becomes a thumbnail-only Post. A fail payload carries Instagram's own
// error text, attached to err for the error card.
func (a *App) oembedFallback(shortcode string, err *AppError) (Post, bool) {
	status, body, ok := a.proxyRawGet("oembed", shortcode, a.cfg.oembedURL(shortcode), map[string]string{
		"User-Agent":  instagramAppUA,
		"Accept":      "*/*",
		"X-IG-App-ID": instagramAppID,
	})
	if !ok || status == 404 || !gjson.Valid(body) {
		return Post{}, false
	}
	if p, ok := parseOembedPost(shortcode, body); ok {
		return p, true
	}
	if gjson.Get(body, "status").String() == "fail" {
		t := gjson.Get(body, "title").String()
		if msg := gjson.Get(body, "message").String(); msg != "" && t != "" {
			err.CardReason, err.CardTitle, err.CardDesc = msg, t, gjson.Get(body, "description").String()
		}
	}
	return Post{}, false
}

// oembedSharedByRE pulls the display name out of the embed blockquote footer:
// "A post shared by {name} (@{handle})".
var oembedSharedByRE = regexp.MustCompile(`A post shared by (.*?) \(@`)

// parseOembedPost builds a thumbnail-only Post from an oembed success
// payload: no video URL, profile pic, or stats are available there.
func parseOembedPost(shortcode, body string) (Post, bool) {
	root := gjson.Parse(body)
	username := root.Get("author_name").String()
	thumb := normalizeCDNHost(root.Get("thumbnail_url").String())
	if username == "" || thumb == "" {
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
			Kind:      "image", // oembed carries no video URL; reels degrade to a thumbnail card
			URL:       thumb,
			Thumbnail: thumb,
			Width:     uintOf(root, "thumbnail_width"),
			Height:    uintOf(root, "thumbnail_height"),
		}},
	}, true
}
