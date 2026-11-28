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
	if s.pending > 0 {
		return s.rttNs * float64(s.pending+1)
	}
	return s.rttNs * math.Exp(-float64(now.Sub(s.rttAt))/float64(sessionRTTDecay))
}

type egressAddrs struct {
	mu    sync.Mutex
	down  []time.Time
	inUse []int
}

func newEgressAddrs(size int) *egressAddrs {
	return &egressAddrs{down: make([]time.Time, size), inUse: make([]int, size)}
}

func (e *egressAddrs) acquire(failed int, now time.Time) (addr int, rested bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if failed >= 0 && failed < len(e.down) {
		e.down[failed] = now.Add(egressAddrCooldown)
	}
	var free, shared []int
	minUse := math.MaxInt
	for i, until := range e.down {
		if until.After(now) {
			continue
		}
		if e.inUse[i] == 0 {
			free = append(free, i)
		} else if e.inUse[i] < minUse {
			minUse = e.inUse[i]
			shared = append(shared[:0], i)
		} else if e.inUse[i] == minUse {
			shared = append(shared, i)
		}
	}
	switch {
	case len(free) > 0:
		addr, rested = free[rand.IntN(len(free))], true
	case len(shared) > 0:
		addr, rested = shared[rand.IntN(len(shared))], true
	default:
		var earliest []int
		var best time.Time
		for i, until := range e.down {
			if len(earliest) == 0 || until.Before(best) {
				best = until
				earliest = append(earliest[:0], i)
			} else if until.Equal(best) {
				earliest = append(earliest, i)
			}
		}
		addr = earliest[rand.IntN(len(earliest))]
	}
	e.inUse[addr]++
	return addr, rested
}

func (e *egressAddrs) release(addr int) {
	if e == nil || addr < 0 {
		return
	}
	e.mu.Lock()
	if addr < len(e.inUse) && e.inUse[addr] > 0 {
		e.inUse[addr]--
	}
	e.mu.Unlock()
}

type SessionPool struct {
	sessions  []*Session
	cfg       Config
	mu        sync.Mutex
	tlsCache  tls.ClientSessionCache
	addrs     *egressAddrs
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	newClient func(proxy *url.URL) *http.Client
}

func (p *SessionPool) nextProxy(failed int) (*url.URL, int, bool) {
	if p.addrs == nil {
		return proxyURL(p.cfg.ProxyUser, p.cfg.ProxyPass, newSessionID()), -1, false
	}
	addr, rested := p.addrs.acquire(failed, time.Now())
	return &url.URL{Scheme: "http", User: url.User("a" + strconv.Itoa(addr)), Host: "egress"}, addr, rested
}

func newEgressPool(socket, prefix string, addrs *egressAddrs) *SessionPool {
	if addrs == nil {
		addrs = newEgressAddrs(egressPoolSize)
	}
	pool := &SessionPool{
		tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2),
		addrs:    addrs,
		dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	for i := range proxySessionCount {
		proxy, addr, _ := pool.nextProxy(-1)
		pool.sessions = append(pool.sessions, &Session{name: prefix + strconv.Itoa(i+1), addr: addr, client: buildSessionClient(proxy, pool)})
	}
	return pool
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg, tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2)}
	if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
		for i := range proxySessionCount {
			proxy, _, _ := pool.nextProxy(-1)
			pool.sessions = append(pool.sessions, &Session{name: "us-" + strconv.Itoa(i+1), addr: -1, client: buildSessionClient(proxy, pool)})
		}
	}

	if len(pool.sessions) == 0 {
		slog.Warn("no proxy sessions configured", "hint", "set PROXY_USERNAME and PROXY_PASSWORD")
	}
	return pool
}

func buildSessionClient(proxy *url.URL, pool *SessionPool) *http.Client {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
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
	s.pending = max(s.pending-1, 0)
	retired := s.client != client
	if !retired && observe {
		sample := max(float64(rtt), float64(time.Millisecond))
		if s.rttAt.IsZero() {
			s.rttNs, s.rttAt = sample, now
		} else {
			decay := math.Exp(-float64(now.Sub(s.rttAt)) / float64(sessionRTTDecay))
			if sample > s.rttNs {
				s.rttNs = sample
			} else {
				s.rttNs = s.rttNs*decay + sample*(1-decay)
			}
			s.rttAt = now
		}
	}
	s.mu.Unlock()
	if retired && client != nil {
		client.CloseIdleConnections()
	}
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
	proxy, addr, rested := p.nextProxy(failed)
	next := buildSessionClient(proxy, p)
	if p.newClient != nil {
		next = p.newClient(proxy)
	}
	s.mu.Lock()
	if s.client != client {
		s.mu.Unlock()
		p.addrs.release(addr)
		return false
	}
	s.client, s.addr = next, addr
	s.rttNs, s.rttAt = 0, time.Time{}
	if !rested {
		if until := time.Now().Add(rotateCooldown); until.After(s.cooldownUntil) {
			s.cooldownUntil = until
		}
	}
	s.mu.Unlock()
	p.addrs.release(failed)
	client.CloseIdleConnections()
	return true
}
