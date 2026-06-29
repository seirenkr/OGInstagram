package main

import (
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

type Session struct {
	name string

	mu         sync.Mutex
	proxyURL   string
	client     *http.Client
	sessionID  string
	decodoUser string
	decodoPass string

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
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg}
	now := time.Now()
	add := func(s *Session) {
		client, err := buildSessionClient(s.proxyURL)
		if err != nil {
			slog.Warn("proxy_skip", "name", s.name, "err", err.Error())
			return
		}
		s.client = client
		s.windowStart = now
		pool.sessions = append(pool.sessions, s)
	}

	if cfg.DecodoUser != "" && cfg.DecodoPass != "" {
		for i := 0; i < proxySessionCount; i++ {
			id := newSessionID()
			add(&Session{
				name:       "us-" + id,
				proxyURL:   decodoProxyURL(cfg.DecodoUser, cfg.DecodoPass, id),
				sessionID:  id,
				decodoUser: cfg.DecodoUser,
				decodoPass: cfg.DecodoPass,
			})
		}
	}

	if len(pool.sessions) == 0 {
		slog.Warn("proxy_pool_empty", "hint", "set DECODO_USERNAME and DECODO_PASSWORD")
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

	var eligible []*Session
	for _, s := range p.sessions {
		if exclude != nil && exclude[s] {
			continue
		}
		s.mu.Lock()
		s.resetBucketWindowLocked(now)
		ok := !s.cooldownUntil.After(now) && s.used < p.cfg.HourlyLimit
		s.mu.Unlock()
		if ok {
			eligible = append(eligible, s)
		}
	}

	var picked []*Session
	if len(eligible) <= count {
		picked = eligible
	} else {
		sort.Slice(eligible, func(i, j int) bool {
			return ewmaRank(eligible[i]) < ewmaRank(eligible[j])
		})
		fastCount := count - fetchRaceExplorers
		if fastCount < 0 {
			fastCount = 0
		}
		picked = append(picked, eligible[:fastCount]...)
		rest := append([]*Session(nil), eligible[fastCount:]...)
		rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
		for _, s := range rest {
			if len(picked) >= count {
				break
			}
			picked = append(picked, s)
		}
	}

	for _, s := range picked {
		s.mu.Lock()
		s.used++
		s.mu.Unlock()
	}
	return picked
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
	slog.Info("proxy_rotated", "session", s.name, "reason", reason)
}

func (p *SessionPool) rotate(s *Session) {
	s.mu.Lock()
	if s.decodoUser != "" {
		s.sessionID = newSessionID()
		s.proxyURL = decodoProxyURL(s.decodoUser, s.decodoPass, s.sessionID)
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
