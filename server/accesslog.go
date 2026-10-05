package main

import (
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// accessLog writes one line per request in nginx's predefined "combined" format:
//
//	$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"
//
// https://nginx.org/en/docs/http/ngx_http_log_module.html#log_format
type accessLog struct {
	mu  sync.Mutex
	out io.Writer
}

type loggingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *loggingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *loggingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (l *accessLog) write(r *http.Request, clientIP string, started time.Time, w *loggingWriter) {
	status := responseStatus(w.status)
	var b strings.Builder
	b.WriteString(logField(clientIP))
	b.WriteString(" - - [")
	b.WriteString(started.UTC().Format("02/Jan/2006:15:04:05 -0700"))
	b.WriteString(`] "`)
	b.WriteString(logEscape(r.Method + " " + r.RequestURI + " " + r.Proto))
	b.WriteString(`" `)
	b.WriteString(strconv.Itoa(status))
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(w.bytes, 10))
	b.WriteString(` "`)
	b.WriteString(logField(r.Referer()))
	b.WriteString(`" "`)
	b.WriteString(logField(r.UserAgent()))
	b.WriteString("\"\n")
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.out, b.String())
}

// A handler that never writes still sends 200, as net/http does.
func responseStatus(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

// nginx writes an empty variable as "-".
func logField(s string) string {
	if s == "" {
		return "-"
	}
	return logEscape(s)
}

// nginx's default escaping: '"', '\', and bytes below 32 or above 126 become \xXX.
func logEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c < 32 || c > 126 {
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// clientAddress is $remote_addr after nginx's realip module: the visitor's IP
// from CF-Connecting-IP when the peer is the trusted connector, else the peer.
func (g *Gateway) clientAddress(r *http.Request) string {
	if g.trustedPeer(r) {
		if addr, err := netip.ParseAddr(r.Header.Get("CF-Connecting-IP")); err == nil {
			return addr.Unmap().String()
		}
	}
	if peer := peerAddress(r); peer.IsValid() {
		return peer.String()
	}
	return ""
}
