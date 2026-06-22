package main

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/tidwall/gjson"
)

func (c Config) oembedURL(shortcode string) string {
	q := url.Values{}
	q.Set("url", instagramOrigin+"/p/"+shortcode+"/")
	return instagramOrigin + "/api/v1/oembed/?" + q.Encode()
}

func (a *App) oembedRefine(shortcode string) (reason, title, desc string, override bool) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.oembedURL(shortcode), nil)
	if err != nil {
		return "", "", "", false
	}
	req.Header.Set("User-Agent", instagramAppUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-IG-App-ID", instagramAppID)

	resp, err := a.direct.Do(req)
	if err != nil {
		return "", "", "", false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	resp.Body.Close()
	body := string(raw)

	if resp.StatusCode == 404 || !gjson.Valid(body) {
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
