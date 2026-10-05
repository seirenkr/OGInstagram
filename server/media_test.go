package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"testing"
)

func mediaTestResponse(r *http.Request, status int, headers http.Header, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}

func TestTrustedMediaDestinations(t *testing.T) {
	for _, raw := range []string{"https://cdninstagram.com/media", "https://scontent.cdninstagram.com/media", "https://scontent.fbcdn.net/media", "https://FBCDN.NET./media"} {
		u, err := url.Parse(raw)
		if err != nil || !trustedMediaURL(u) {
			t.Errorf("trusted URL rejected: %s", raw)
		}
	}
	for _, raw := range []string{"http://cdninstagram.com/media", "https://cdninstagram.com:443/media", "https://user@fbcdn.net/media", "https://fbcdn.net.evil.example/media", "https://evilcdninstagram.com/media", "https://127.0.0.1/media", "https://metadata.google.internal/media"} {
		u, err := url.Parse(raw)
		if err == nil && trustedMediaURL(u) {
			t.Errorf("unsafe URL accepted: %s", raw)
		}
	}
}
