package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var egressHosts = map[string]bool{"i.instagram.com:443": true, "www.instagram.com:443": true}

func runEgress(socket, poolFile string) error {
	pool, err := readEgressPool(poolFile)
	if err != nil {
		return err
	}
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	slog.Info("ipv6 egress listening", "socket", socket, "addresses", len(pool))
	srv := &http.Server{Handler: egressHandler(pool), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	errC := make(chan error, 1)
	go func() { errC <- srv.Serve(ln) }()
	select {
	case err := <-errC:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = srv.Close()
	}
	serveErr := <-errC
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return shutdownErr
}

func readEgressPool(path string) ([]net.IP, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pool []net.IP
	for _, line := range strings.Fields(string(raw)) {
		if ip := net.ParseIP(line); ip != nil && ip.To4() == nil {
			pool = append(pool, ip)
		}
	}
	if len(pool) == 0 {
		return nil, errors.New("no IPv6 addresses in " + path)
	}
	return pool, nil
}

func egressHandler(pool []net.IP) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || !egressHosts[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		source, ok := egressSource(pool, r.Header.Get("Proxy-Authorization"))
		if !ok {
			http.Error(w, "unknown source", http.StatusProxyAuthRequired)
			return
		}
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: source}, Timeout: 3 * time.Second}
		upstream, err := d.DialContext(r.Context(), "tcp6", r.Host)
		if err != nil {
			slog.Warn("ipv6 egress dial failed", "source", source.String(), "host", r.Host, "error", err)
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		client, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			client.Close()
			upstream.Close()
			return
		}
		var clientSrc io.Reader = client
		if buffered.Reader.Buffered() > 0 {
			clientSrc = buffered.Reader
		}
		go pipe(upstream, clientSrc, client)
		pipe(client, upstream, upstream)
	})
}

func egressSource(pool []net.IP, auth string) (net.IP, bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	user, _, _ := strings.Cut(string(raw), ":")
	digits, isIndex := strings.CutPrefix(user, "a")
	i, convErr := strconv.Atoi(digits)
	if err != nil || !isIndex || convErr != nil || i < 0 {
		return nil, false
	}
	return pool[i%len(pool)], true
}

func pipe(dst io.WriteCloser, src io.Reader, other io.Closer) {
	_, _ = io.Copy(dst, src)
	dst.Close()
	other.Close()
}
