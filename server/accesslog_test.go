package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestAccessLogUsesNginxCombinedFormat(t *testing.T) {
	g := &Gateway{cfg: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.30.0.3/32")}}}
	var out bytes.Buffer
	l := &accessLog{out: &out}
	r := httptest.NewRequest("GET", `/p/ABC?x="y"`, nil)
	r.RemoteAddr = "172.30.0.3:5555"
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	r.Header.Set("User-Agent", "Discordbot/2.0 \"évil\"\n")
	w := &loggingWriter{ResponseWriter: httptest.NewRecorder()}
	w.WriteHeader(http.StatusFound)
	_, _ = w.Write([]byte("hello"))
	l.write(r, g.clientAddress(r), time.Date(2026, 10, 5, 10, 39, 20, 0, time.UTC), w)
	want := `203.0.113.7 - - [05/Oct/2026:10:39:20 +0000] "GET /p/ABC?x=\x22y\x22 HTTP/1.1" 302 5 "-" "Discordbot/2.0 \x22\xC3\xA9vil\x22\x0A"` + "\n"
	if out.String() != want {
		t.Fatalf("got  %q\nwant %q", out.String(), want)
	}

	// An untrusted peer cannot choose the logged address.
	r.RemoteAddr = "198.51.100.9:1234"
	if got := g.clientAddress(r); got != "198.51.100.9" {
		t.Errorf("untrusted peer logged as %q", got)
	}
}
