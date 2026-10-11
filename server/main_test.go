package main

import (
	"strings"
	"testing"
	"time"
)

func TestStatusEndpointsHideUpstreamErrors(t *testing.T) {
	g := testGateway(t)
	exp := time.Now().Add(time.Hour)
	g.app.posts.storeLocal("GEO123", &cacheEntry[Post]{err: igErr(451, errorCodeGeoBlocked, "geo detail"), expiresAt: exp})
	g.app.posts.storeLocal("DOWN12", &cacheEntry[Post]{err: igErr(502, errorCodeUpstream, "external helper had no post"), expiresAt: exp})
	for _, tt := range []struct {
		shortcode, retry, body string
		status                 int
	}{
		{"GEO123", "", "Record not found", 451},
		{"DOWN12", "60", "Temporarily unavailable", 503},
	} {
		code := statusSnowcode("p", tt.shortcode, 0, false, false)
		for _, path := range []string{"/api/v1/statuses/" + code, "/users/alice/statuses/" + code} {
			r := serveApp(t, g.app, publicRequest("GET", path))
			if r.status != tt.status || r.headers["Retry-After"] != tt.retry || !strings.Contains(string(r.body), tt.body) {
				t.Errorf("%s = %d Retry-After %q %s", path, r.status, r.headers["Retry-After"], r.body)
			}
		}
	}
}
