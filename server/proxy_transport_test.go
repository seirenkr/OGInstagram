package main

import (
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyTransportResumesTLSAcrossClients(t *testing.T) {
	const bodyBytes = 128 << 10
	resumed := make(chan bool, 2)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("expected HTTP/2")
		}
		resumed <- r.TLS.DidResume
		_, _ = io.WriteString(w, strings.Repeat("x", bodyBytes))
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	pool := newSessionPool(Config{ProxyUser: "test", ProxyPass: "test"})
	pool.budgetLeaseExpires = time.Now().Add(time.Hour)
	pool.budgetLeaseRemaining = 1 << 20
	t.Cleanup(func() {
		for _, session := range pool.sessions {
			session.getClient().CloseIdleConnections()
		}
	})

	for attempt := range 2 {
		client, err := buildSessionClient("http://proxy.invalid:823", pool)
		if err != nil {
			t.Fatal(err)
		}
		transport := client.Transport.(*http.Transport)
		transport.Proxy = nil
		transport.TLSClientConfig.RootCAs = roots
		client.Timeout = 5 * time.Second
		response, err := client.Get(upstream.URL)
		if err != nil {
			client.CloseIdleConnections()
			t.Fatal(err)
		}
		got, readErr := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		client.CloseIdleConnections()
		if readErr != nil || got != bodyBytes {
			t.Fatalf("body larger than the receive window: bytes=%d, error=%v", got, readErr)
		}
		if got := <-resumed; got != (attempt == 1) {
			t.Fatalf("attempt %d: TLS resumed=%v", attempt, got)
		}
	}
}
