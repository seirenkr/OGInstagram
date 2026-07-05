package main

import (
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

type Session struct {
	name string

	mu        sync.Mutex
	proxyURL  string
	client    *http.Client
	sessionID string

	ewmaMs        float64
	hasEWMA       bool
	windowStart   time.Time
	used          int
	cooldownUntil time.Time
}

type SessionPool struct {
	sessions []*Session
	cfg      Config
	mu       sync.Mutex

	globalWindowStart time.Time
	globalUsed        int
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg}
	now := time.Now()
	add := func(s *Session) {
		client, err := buildSessionClient(s.proxyURL)
		if err != nil {
			slog.Warn("skipped proxy session", "name", s.name, "err", err.Error())
			return
		}
		s.client = client
		s.windowStart = now
		pool.sessions = append(pool.sessions, s)
	}

	if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
		for i := 0; i < proxySessionCount; i++ {
			id := newSessionID()
			add(&Session{
				name:      "us-" + id,
				proxyURL:  proxyURL(cfg.ProxyUser, cfg.ProxyPass, id),
				sessionID: id,
			})
		}
	}

	if len(pool.sessions) == 0 {
		slog.Warn("no proxy sessions configured", "hint", "set PROXY_USERNAME and PROXY_PASSWORD")
	}
	return pool
}

func buildSessionClient(proxyURL string) (*http.Client, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(parsed),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   fetchTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport}, nil
}

func (p *SessionPool) pick(count int, exclude map[*Session]bool) []*Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	// Global hourly cap, counted on successful (data-downloaded) requests only.
	p.resetGlobalWindowLocked(now)
	if p.cfg.GlobalHourlyLimit > 0 && p.globalUsed >= p.cfg.GlobalHourlyLimit {
		return nil
	}

	var eligible []*Session
	for _, s := range p.sessions {
		if exclude != nil && exclude[s] {
			continue
		}
		s.mu.Lock()
		s.resetBucketWindowLocked(now)
		ok := !s.cooldownUntil.After(now) && s.used < defaultProxyHourlyLimit
		s.mu.Unlock()
		if ok {
			eligible = append(eligible, s)
		}
	}

	// ponytail: fastest-EWMA pick only; sessions without an EWMA rank first
	// (-1), so new/rotated-in sessions still get explored via the hedge pick.
	picked := eligible
	if len(eligible) > count {
		sort.Slice(eligible, func(i, j int) bool {
			return ewmaRank(eligible[i]) < ewmaRank(eligible[j])
		})
		picked = eligible[:count]
	}

	for _, s := range picked {
		s.mu.Lock()
		s.used++
		s.mu.Unlock()
	}
	return picked
}

func (p *SessionPool) resetGlobalWindowLocked(now time.Time) {
	if p.globalWindowStart.IsZero() || now.Sub(p.globalWindowStart) >= time.Hour {
		p.globalWindowStart = now
		p.globalUsed = 0
	}
}

// countRequest records one successful (data-downloaded) proxy request against
// the hourly budget. Race losers cancelled mid-flight are not counted.
func (p *SessionPool) countRequest() {
	p.mu.Lock()
	p.resetGlobalWindowLocked(time.Now())
	p.globalUsed++
	p.mu.Unlock()
}

func (p *SessionPool) overBudget() bool {
	if p.cfg.GlobalHourlyLimit <= 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetGlobalWindowLocked(time.Now())
	return p.globalUsed >= p.cfg.GlobalHourlyLimit
}

func ewmaRank(s *Session) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasEWMA {
		return -1
	}
	return s.ewmaMs
}

func (p *SessionPool) recordLatency(s *Session, d time.Duration) {
	ms := float64(d.Milliseconds())
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasEWMA {
		s.ewmaMs = ms
		s.hasEWMA = true
		return
	}
	s.ewmaMs = s.ewmaMs*(1-ewmaAlpha) + ms*ewmaAlpha
}

func (s *Session) getClient() *http.Client {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	return c
}

func (p *SessionPool) fail(s *Session, reason string) {
	p.rotate(s)
	until := time.Now().Add(rotateCooldown)
	s.mu.Lock()
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.mu.Unlock()
	slog.Info("rotated proxy session", "session", s.name, "reason", reason)
}

func (p *SessionPool) rotate(s *Session) {
	s.mu.Lock()
	if p.cfg.ProxyUser != "" {
		s.sessionID = newSessionID()
		s.proxyURL = proxyURL(p.cfg.ProxyUser, p.cfg.ProxyPass, s.sessionID)
	}
	proxyURL := s.proxyURL
	s.mu.Unlock()

	client, err := buildSessionClient(proxyURL)
	if err != nil {
		return
	}
	s.mu.Lock()
	old := s.client
	s.client = client
	s.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

func (s *Session) resetBucketWindowLocked(now time.Time) {
	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= time.Hour {
		s.windowStart = now
		s.used = 0
	}
}
