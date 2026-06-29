package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	pk := shortcodeToPK(shortcode)
	variables, _ := json.Marshal(map[string]string{"media_id": pk})
	lsd := newLSD()
	form := url.Values{}
	form.Set("lsd", lsd)
	form.Set("variables", string(variables))
	form.Set("doc_id", instagramWebLoggedOutDocID)
	form.Set("server_timestamps", "true")
	return gqlSpec{
		name:   "post",
		target: shortcode,
		method: http.MethodPost,
		url:    instagramOrigin + "/api/graphql",
		body:   form.Encode(),
		headers: map[string]string{
			"User-Agent":         instagramWebUA,
			"Content-Type":       "application/x-www-form-urlencoded",
			"Sec-Fetch-Site":     "same-origin",
			"X-FB-Friendly-Name": "PolarisLoggedOutDesktopWWWPostRootContentQuery",
			"X-FB-LSD":           lsd,
			"X-Requested-With":   "XMLHttpRequest",
		},
	}
}

func (a *App) raceFetch(spec gqlSpec) (string, *AppError) {
	sessions := a.pool.pick(fetchRaceCount, nil)
	if len(sessions) == 0 {
		if len(a.pool.sessions) == 0 {
			return "", igErr(503, reasonConnection, "Instagram proxy sessions are not configured")
		}
		return "", ephemeralErr(503, reasonConnection, "all Instagram proxy sessions are rate limited or cooling down")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		body string
		err  *AppError
	}
	results := make(chan result, fetchRaceCount+fetchHedgeCount)
	racing := map[*Session]bool{}
	var pending int

	launch := func(s *Session) {
		racing[s] = true
		pending++
		go func() {
			body, err := a.attemptFetch(ctx, spec, s)
			results <- result{body, err}
		}()
	}
	for _, s := range sessions {
		launch(s)
	}

	hedged := false
	hedge := func() {
		if hedged {
			return
		}
		hedged = true
		for _, s := range a.pool.pick(fetchHedgeCount, racing) {
			launch(s)
		}
	}

	timer := time.NewTimer(fetchHedgeDelay)
	defer timer.Stop()

	var lastErr *AppError
	for pending > 0 {
		select {
		case <-timer.C:
			hedge()
		case r := <-results:
			pending--
			if r.err == nil {
				return r.body, nil
			}

			// Prefer a permanent error (real 404) over a transient one (a throttled IP's 401).
			if lastErr == nil || (isTransient(lastErr.Reason) && !isTransient(r.err.Reason)) {
				lastErr = r.err
			}
			if pending == 0 && !hedged {
				hedge()
			}
		}
	}
	if lastErr == nil {
		lastErr = igErr(502, reasonClientError, "Instagram fetch failed")
	}
	return "", lastErr
}

func (a *App) attemptFetch(ctx context.Context, spec gqlSpec, s *Session) (body string, ferr *AppError) {
	started := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	defer func() {
		ms := time.Since(started).Milliseconds()
		endpoint := spec.url
		if i := strings.IndexByte(endpoint, '?'); i >= 0 {
			endpoint = endpoint[:i]
		}
		msg := spec.method + " " + endpoint
		switch {
		case ferr == nil:
			slog.Info(msg, "op", spec.name, "target", spec.target, "status", 200, "session", s.name, "ms", ms, "bytes", len(body))
		case ferr.Status != 499:
			slog.Warn(msg, "op", spec.name, "target", spec.target, "status", ferr.Status,
				"reason", ferr.Reason, "session", s.name, "ms", ms, "detail", ferr.Message)
		}
	}()

	var bodyReader io.Reader
	if spec.body != "" {
		bodyReader = strings.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(reqCtx, spec.method, spec.url, bodyReader)
	if err != nil {
		return "", newAppError(500, err.Error())
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
			return "", newAppError(499, "cancelled")
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
	return bodyText, nil
}

func (a *App) proxyRawGet(rawURL string, headers map[string]string) (int, string, bool) {
	sessions := a.pool.pick(1, nil)
	if len(sessions) == 0 {
		return 0, "", false
	}
	s := sessions[0]
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
		return 0, "", false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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

func (a *App) oembedRefine(shortcode string) (reason, title, desc string, override bool) {
	status, body, ok := a.proxyRawGet(a.cfg.oembedURL(shortcode), map[string]string{
		"User-Agent":  instagramAppUA,
		"Accept":      "*/*",
		"X-IG-App-ID": instagramAppID,
	})
	if !ok || status == 404 || !gjson.Valid(body) {
		return "", "", "", false
	}
	if gjson.Get(body, "status").String() == "fail" {
		t := gjson.Get(body, "title").String()
		if msg := gjson.Get(body, "message").String(); msg != "" && t != "" {
			return msg, t, gjson.Get(body, "description").String(), true
		}
	}
	return "", "", "", false
}
