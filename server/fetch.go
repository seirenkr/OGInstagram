package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"html"
	"io"
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
	lsd := rand.Text()
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

			"User-Agent":         "Mozilla/5.0",
			"Accept":             "*/*",
			"Content-Type":       "application/x-www-form-urlencoded",
			"X-FB-Friendly-Name": "PolarisPostRootQuery",
			"X-FB-LSD":           lsd,
		},
	}
}

func (a *App) raceFetch(ctx context.Context, spec gqlSpec) (string, *AppError) {
	if ctx.Err() != nil {
		return "", igErr(499, "", "cancelled")
	}
	primary, pickReason := a.pool.pick(ctx, nil)
	if primary == nil {
		if pickReason == reasonBudgetBackend {
			return "", ephemeralErr(503, reasonBudgetBackend, "hourly request budget is temporarily unavailable")
		}
		if pickReason == reasonBudgetExceeded {
			return "", ephemeralErr(503, reasonBudgetExceeded, "hourly request budget reached")
		}
		if len(a.pool.sessions) == 0 {
			return "", igErr(503, reasonConnection, "Instagram proxy sessions are not configured")
		}
		return "", ephemeralErr(503, reasonConnection, "all Instagram proxy sessions are rate limited or cooling down")
	}

	return hedgedPair(ctx, func(ctx context.Context) (string, *AppError) {
		return a.attemptFetch(ctx, spec, primary)
	}, func() attempt {
		secondary, _ := a.pool.pick(ctx, primary)
		if secondary == nil {
			return nil
		}
		return func(ctx context.Context) (string, *AppError) { return a.attemptFetch(ctx, spec, secondary) }
	})
}

type attempt func(context.Context) (string, *AppError)

func hedgedPair(parent context.Context, primary attempt, hedge func() attempt) (string, *AppError) {
	type result struct {
		value string
		err   *AppError
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	results := make(chan result, 2)
	launch := func(f attempt) {
		go func() {
			v, err := f(ctx)
			results <- result{v, err}
		}()
	}
	launch(primary)

	timer := time.NewTimer(fetchHedgeDelay)
	defer timer.Stop()
	timerC := timer.C
	pending := 1
	hedged := false
	launchHedge := func() {
		if hedged || ctx.Err() != nil {
			return
		}
		hedged = true
		timerC = nil
		if next := hedge(); next != nil {
			pending++
			launch(next)
		}
	}

	var lastErr *AppError
	for pending > 0 {
		select {
		case <-ctx.Done():
			return "", igErr(499, "", "cancelled")
		case <-timerC:
			launchHedge()
		case r := <-results:
			pending--
			if r.err == nil {
				return r.value, nil
			}
			if lastErr == nil || (isTransient(lastErr.Reason) && !isTransient(r.err.Reason)) {
				lastErr = r.err
			}
			if pending == 0 {
				launchHedge()
			}
		}
	}
	if lastErr == nil {
		lastErr = igErr(502, reasonClientError, "Instagram fetch failed")
	}
	return "", lastErr
}

func logOutbound(ctx context.Context, op, target, session, method, rawURL string, started time.Time, status, bytes int, ferr *AppError) {
	endpoint := rawURL
	if i := strings.IndexByte(endpoint, '?'); i >= 0 {
		endpoint = endpoint[:i]
	}
	parsedEndpoint, _ := url.Parse(endpoint)
	host, path := parsedEndpoint.Hostname(), parsedEndpoint.Path
	ms := time.Since(started).Milliseconds()
	msg := method + " " + endpoint + " " + strconv.Itoa(status) + " " + strconv.FormatInt(ms, 10) + "ms"
	if ferr == nil {
		logger(ctx).InfoContext(ctx, msg, "event", "outbound_request", "op", op, "target", target,
			"method", method, "host", host, "path", path, "status", status, "session", session, "ms", ms, "bytes", bytes)
		return
	}
	if ferr.Status == 499 {
		return
	}
	if ferr.Reason != "" {
		msg += " reason=" + ferr.Reason
	}
	logger(ctx).WarnContext(ctx, msg, "event", "outbound_request", "op", op, "target", target, "status", status,
		"method", method, "host", host, "path", path, "reason", ferr.Reason, "session", session, "ms", ms, "detail", ferr.Message)
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
		logOutbound(ctx, spec.name, spec.target, s.name, spec.method, spec.url, started, status, len(body), ferr)
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

	resp, err := s.getClient().Do(req)
	if err != nil {

		if ctx.Err() != nil {
			return "", igErr(499, "", "cancelled")
		}
		a.pool.fail(ctx, s, reasonConnection)
		return "", igErr(502, reasonConnection, err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if ctx.Err() != nil {
		return "", igErr(499, "", "cancelled")
	}
	if readErr != nil {
		a.pool.fail(ctx, s, reasonConnection)
		return "", igErr(502, reasonConnection, readErr.Error())
	}
	bodyText := string(raw)

	if resp.StatusCode >= 400 {
		msg := ""
		if gjson.Valid(bodyText) {
			msg = strings.TrimSpace(gjson.Get(bodyText, "message").String())
		}
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
			a.pool.fail(ctx, s, reason)
		}
		return "", igErr(resp.StatusCode, reason, msg)
	}

	if strings.HasPrefix(bodyText, "<!DOCTYPE html") || strings.Contains(bodyText, "require_login") {
		a.pool.fail(ctx, s, reasonLoginRequired)
		return "", igErr(401, reasonLoginRequired, "Instagram requires login to view this content")
	}

	if !gjson.Valid(bodyText) {
		a.pool.fail(ctx, s, reasonJSONDecode)
		return "", igErr(502, reasonJSONDecode, "Instagram returned an unreadable response")
	}

	parsed := gjson.Parse(bodyText)
	if parsed.Get("status").String() == "fail" {
		msg := strings.TrimSpace(parsed.Get("message").String())
		if msg == "" {
			msg = "Instagram request failed"
		}
		return "", igErr(502, reasonGraphql, msg)
	}

	a.pool.recordLatency(s, time.Since(started))
	return bodyText, nil
}

func (a *App) proxyRawGet(parent context.Context, op, target, rawURL string, headers map[string]string) (int, string, bool) {
	s, _ := a.pool.pick(parent, nil)
	if s == nil {
		return 0, "", false
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, fetchTimeout)
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
		if parent.Err() != nil {
			return 0, "", false
		}
		logOutbound(ctx, op, target, s.name, http.MethodGet, rawURL, started, 502, 0, igErr(502, reasonConnection, err.Error()))
		return 0, "", false
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		logOutbound(ctx, op, target, s.name, http.MethodGet, rawURL, started, 502, len(raw), igErr(502, reasonConnection, readErr.Error()))
		return 0, "", false
	}
	logOutbound(ctx, op, target, s.name, http.MethodGet, rawURL, started, resp.StatusCode, len(raw), nil)
	return resp.StatusCode, string(raw), true
}

func (c Config) oembedURL(shortcode string) string {
	q := url.Values{}
	q.Set("url", instagramOrigin+"/p/"+shortcode+"/")
	return instagramOrigin + "/api/v1/oembed/?" + q.Encode()
}

func (a *App) oembedFallback(ctx context.Context, shortcode string, err *AppError) (Post, bool) {
	status, body, ok := a.proxyRawGet(ctx, "oembed", shortcode, a.cfg.oembedURL(shortcode), map[string]string{
		"User-Agent":  instagramAppUA,
		"Accept":      "*/*",
		"X-IG-App-ID": instagramAppID,
	})
	if !ok || !gjson.Valid(body) {
		return Post{}, false
	}
	if status >= 200 && status < 300 {
		if p, ok := parseOembedPost(shortcode, body); ok {
			return p, true
		}
	}
	root := gjson.Parse(body)
	if root.Get("status").String() == "fail" {
		msg := root.Get("message").String()

		if msg == "geoblock_required" {
			msg = reasonGeoBlocked
			err.Reason = reasonGeoBlocked
			err.Status = 451
		}
		if title := root.Get("title").String(); msg != "" && title != "" {
			err.CardReason, err.CardTitle, err.CardDesc = msg, title, root.Get("description").String()
		}
	}
	return Post{}, false
}

var oembedSharedByRE = regexp.MustCompile(`A post shared by (.*?) \(@`)

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
			Kind:      "image",
			URL:       thumb,
			Thumbnail: thumb,
			Width:     uintOf(root, "thumbnail_width"),
			Height:    uintOf(root, "thumbnail_height"),
		}},
	}, true
}
