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
	addr int

	mu     sync.Mutex
	client *http.Client

	rttNs   float64
	rttAt   time.Time
	pending int

	cooldownUntil time.Time
}

func (s *Session) costLocked(now time.Time) float64 {
	if s.rttAt.IsZero() {
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
	addrDown []time.Time
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p *SessionPool) nextProxy(failed int) (*url.URL, int) {
	if p.addrDown == nil {
		return proxyURL(p.cfg.ProxyUser, p.cfg.ProxyPass, newSessionID()), -1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if failed >= 0 {
		p.addrDown[failed] = now.Add(egressAddrCooldown)
	}
	var rested []int
	for i, until := range p.addrDown {
		if !until.After(now) {
			rested = append(rested, i)
		}
	}
	addr := rand.IntN(len(p.addrDown))
	if len(rested) > 0 {
		addr = rested[rand.IntN(len(rested))]
	}
	return &url.URL{Scheme: "http", User: url.User("a" + strconv.Itoa(addr)), Host: "egress"}, addr
}

func newEgressPool(socket, prefix string) *SessionPool {
	pool := &SessionPool{
		tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2),
		addrDown: make([]time.Time, egressPoolSize),
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	for i := range proxySessionCount {
		proxy, addr := pool.nextProxy(-1)
		pool.sessions = append(pool.sessions, &Session{name: prefix + strconv.Itoa(i+1), addr: addr, client: buildSessionClient(proxy, pool)})
	}
	return pool
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg, tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2)}
	if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
		for i := range proxySessionCount {
			proxy, _ := pool.nextProxy(-1)
			pool.sessions = append(pool.sessions, &Session{name: "us-" + strconv.Itoa(i+1), addr: -1, client: buildSessionClient(proxy, pool)})
		}
	}

	if len(pool.sessions) == 0 {
		slog.Warn("no proxy sessions configured", "hint", "set PROXY_USERNAME and PROXY_PASSWORD")
	}
	return pool
}

func buildSessionClient(proxy *url.URL, pool *SessionPool) *http.Client {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	dial := dialer.DialContext
	if pool.dial != nil {
		dial = pool.dial
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxy),
		DialContext:         dial,
		ForceAttemptHTTP2:   true,
		DisableCompression:  true,
		TLSHandshakeTimeout: 3 * time.Second,
		TLSClientConfig:     &tls.Config{ClientSessionCache: pool.tlsCache},
		HTTP2: &http.HTTP2Config{
			MaxDecoderHeaderTableSize: 64 << 10,
			MaxReceiveBufferPerStream: 32 << 10,
			SendPingTimeout:           30 * time.Second,
			PingTimeout:               2 * time.Second,
		},
		IdleConnTimeout: 90 * time.Second,
	}
	return &http.Client{Transport: transport}
}

func (p *SessionPool) pick() *Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	ready := make([]*Session, 0, len(p.sessions))
	for _, s := range p.sessions {
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

func (p *SessionPool) fail(s *Session, client *http.Client) bool {
	s.mu.Lock()
	current, failed := s.client == client, s.addr
	s.mu.Unlock()
	if !current {
		return false
	}
	proxy, addr := p.nextProxy(failed)
	next := buildSessionClient(proxy, p)
	s.mu.Lock()
	if s.client != client {
		s.mu.Unlock()
		return false
	}
	s.client, s.addr = next, addr
	s.rttNs, s.rttAt = 0, time.Time{}
	if until := time.Now().Add(rotateCooldown); until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.mu.Unlock()
	client.CloseIdleConnections()
	return true
}
