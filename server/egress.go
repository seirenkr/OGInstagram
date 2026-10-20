package main

import (
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Hosts the IPv6 egress will tunnel to; it must never become an open proxy.
var egressHosts = map[string]bool{"i.instagram.com:443": true, "www.instagram.com:443": true}

// runEgress serves HTTP CONNECT to Instagram on a unix socket, dialing from the
// host's IPv6 pool; the proxy username picks the source address.
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
	return srv.Serve(ln)
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
		h := fnv.New32a()
		h.Write([]byte(r.Header.Get("Proxy-Authorization")))
		source := pool[h.Sum32()%uint32(len(pool))]
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: source}, Timeout: 5 * time.Second}
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
		go pipe(upstream, buffered.Reader, client)
		pipe(client, upstream, upstream)
	})
}

// pipe copies until either side ends, then closes both connections.
func pipe(dst io.WriteCloser, src io.Reader, other io.Closer) {
	_, _ = io.Copy(dst, src)
	dst.Close()
	other.Close()
}
