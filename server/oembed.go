package main

import (
	"net/url"

	"github.com/tidwall/gjson"
)

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
