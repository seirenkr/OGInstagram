package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type Session struct {
	name string

	mu     sync.Mutex
	client *http.Client

	// Peak-EWMA round trip of the current exit IP (as in tower and Finagle): a
	// slower sample replaces it, faster ones pull it down, and it decays toward 0
	// while idle so a once-slow session gets retried. Zero rttAt = unmeasured.
	rttNs   float64
	rttAt   time.Time
	pending int

	cooldownUntil time.Time
}

// costLocked is the expected wait for one more request: rtt × (pending+1).
// A hung exit IP piles up pending requests, so its cost climbs at once
// instead of only after the first timeout comes back.
func (s *Session) costLocked(now time.Time) float64 {
	if s.rttAt.IsZero() {
		// Unmeasured (Finagle's rule): free to try once, but never pile onto its
		// first request, which may be hanging on a dead exit IP.
		return float64(time.Hour) * float64(s.pending)
	}
	rtt := s.rttNs * math.Exp(-float64(now.Sub(s.rttAt))/float64(sessionRTTDecay))
	return rtt * float64(s.pending+1)
}

type SessionPool struct {
	sessions []*Session
	cfg      Config
	mu       sync.Mutex
	tlsCache tls.ClientSessionCache
	// Set for the IPv6 egress pool; nil means the DC proxy.
	proxyURL func() string
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p *SessionPool) nextProxyURL() string {
	if p.proxyURL != nil {
		return p.proxyURL()
	}
	return proxyURL(p.cfg.ProxyUser, p.cfg.ProxyPass, newSessionID())
}

// newEgressPool connects through the IPv6 egress; each session id maps to one source address.
func newEgressPool(socket, prefix string) *SessionPool {
	pool := &SessionPool{
		tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2),
		proxyURL: func() string { return "http://" + newSessionID() + "@egress" },
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	for i := range proxySessionCount {
		client, _ := buildSessionClient(pool.nextProxyURL(), pool)
		pool.sessions = append(pool.sessions, &Session{name: prefix + strconv.Itoa(i+1), client: client})
	}
	return pool
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg, tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2)}
	if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
		for i := range proxySessionCount {
			// A stable slot label: rotation replaces the exit IP behind it.
			name := "us-" + strconv.Itoa(i+1)
			client, err := buildSessionClient(pool.nextProxyURL(), pool)
			if err != nil {
				slog.Warn("proxy session skipped: invalid proxy configuration", "session", name, "error", err)
				continue
			}
			pool.sessions = append(pool.sessions, &Session{name: name, client: client})
		}
	}

	if len(pool.sessions) == 0 {
		slog.Warn("no proxy sessions configured", "hint", "set PROXY_USERNAME and PROXY_PASSWORD")
	}
	return pool
}

func buildSessionClient(proxyURL string, pool *SessionPool) (*http.Client, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	dial := dialer.DialContext
	if pool.dial != nil {
		dial = pool.dial
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(parsed),
		DialContext:         dial,
		ForceAttemptHTTP2:   true,
		DisableCompression:  true,
		TLSHandshakeTimeout: 3 * time.Second,
		TLSClientConfig:     &tls.Config{ClientSessionCache: pool.tlsCache},
		// Close a silent tunnel instead of letting requests hang on it.
		HTTP2: &http.HTTP2Config{
			MaxDecoderHeaderTableSize: 64 << 10,
			// Bound bytes already in flight when an unexpected HTML response is rejected.
			MaxReceiveBufferPerStream: 32 << 10,
			SendPingTimeout:           30 * time.Second,
			PingTimeout:               2 * time.Second,
		},
		IdleConnTimeout: 90 * time.Second,
	}
	return &http.Client{Transport: transport}, nil
}

// www.instagram.com traffic (oEmbed, profile, ruling) is sparse: spread over
// every session it would mostly open new connections, so it keeps to the first
// wwwSessions unless none of them is ready.
const wwwSessions = 3

func (p *SessionPool) pick(www bool) *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	ready := make([]*Session, 0, len(p.sessions))
	for i, s := range p.sessions {
		if www && i == wwwSessions && len(ready) > 0 {
			break
		}
		s.mu.Lock()
		if !s.cooldownUntil.After(now) {
			ready = append(ready, s)
		}
		s.mu.Unlock()
	}
	var picked *Session
	switch len(ready) {
	case 0:
		return nil
	case 1:
		picked = ready[0]
	default:
		// Power of two choices (tower, Finagle, Envoy least-request): compare two
		// random sessions instead of always taking the single best, which spreads
		// load across exit IPs and avoids a herd on one.
		i := rand.IntN(len(ready))
		j := rand.IntN(len(ready) - 1)
		if j >= i {
			j++
		}
		picked = ready[i]
		ready[i].mu.Lock()
		ready[j].mu.Lock()
		if ready[j].costLocked(now) < ready[i].costLocked(now) {
			picked = ready[j]
		}
		ready[j].mu.Unlock()
		ready[i].mu.Unlock()
	}

	picked.mu.Lock()
	picked.pending++
	picked.mu.Unlock()
	return picked
}

// done ends a request picked from s that ran on client. With observe, rtt feeds
// the Peak-EWMA estimate; tower's update rule, weighted by the time since the
// last sample. A retired client's rtt says nothing about the current exit IP.
func (p *SessionPool) done(s *Session, client *http.Client, rtt time.Duration, observe bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = max(s.pending-1, 0)
	if !observe || s.client != client {
		return
	}
	sample := float64(rtt)
	if s.rttAt.IsZero() {
		s.rttNs, s.rttAt = sample, now
		return
	}
	decay := math.Exp(-float64(now.Sub(s.rttAt)) / float64(sessionRTTDecay))
	if current := s.rttNs * decay; sample > current {
		s.rttNs = sample
	} else {
		s.rttNs = current + (sample-current)*(1-decay)
	}
	s.rttAt = now
}

func (s *Session) getClient() *http.Client {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	return c
}

// fail moves s off client to a new exit IP and cools it down. A late failure
// from an already retired client leaves its replacement alone and reports false.
func (p *SessionPool) fail(s *Session, client *http.Client) bool {
	next, err := buildSessionClient(p.nextProxyURL(), p)
	if err != nil {
		slog.Warn("proxy session rotation failed", "session", s.name, "error", err)
	}
	s.mu.Lock()
	if s.client != client {
		s.mu.Unlock()
		return false
	}
	if next != nil {
		s.client = next
		s.rttNs, s.rttAt = 0, time.Time{} // a new exit IP starts unmeasured
	}
	if until := time.Now().Add(rotateCooldown); until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.mu.Unlock()
	if next != nil {
		client.CloseIdleConnections()
	}
	return true
}
