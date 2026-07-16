package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
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
	budget   *http.Client
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg, budget: &http.Client{Timeout: time.Second}}
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
		MaxConnsPerHost:       maxConcurrentFetches,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   fetchTimeout,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport}, nil
}

func (p *SessionPool) pick(ctx context.Context, exclude *Session) (*Session, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	var picked *Session
	bestRank := 0.0
	for _, s := range p.sessions {
		if s == exclude {
			continue
		}
		s.mu.Lock()
		s.resetBucketWindowLocked(now)
		ok := !s.cooldownUntil.After(now) && s.used < defaultProxyHourlyLimit
		rank := s.ewmaMs
		if !s.hasEWMA {
			rank = -1
		}
		s.mu.Unlock()
		if ok && (picked == nil || rank < bestRank) {
			picked, bestRank = s, rank
		}
	}

	if picked != nil {
		if reason := p.reserveBudget(ctx); reason != "" {
			return nil, reason
		}
		picked.mu.Lock()
		picked.used++
		picked.mu.Unlock()
	}
	return picked, ""
}

func (p *SessionPool) reserveBudget(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BudgetURL, nil)
	if err != nil {
		logger(ctx).ErrorContext(ctx, "proxy budget request failed", "event", "proxy_budget_error", "reason", "request", "error", err.Error())
		return reasonBudgetBackend
	}
	resp, err := p.budget.Do(req)
	if err != nil {
		logger(ctx).ErrorContext(ctx, "proxy budget request failed", "event", "proxy_budget_error", "reason", "connection", "error", err.Error())
		return reasonBudgetBackend
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return ""
	case http.StatusTooManyRequests:
		return reasonBudgetExceeded
	default:
		logger(ctx).ErrorContext(ctx, "proxy budget request failed", "event", "proxy_budget_error", "reason", "status", "status", resp.StatusCode)
		return reasonBudgetBackend
	}
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

func (p *SessionPool) fail(ctx context.Context, s *Session, reason string) {
	p.rotate(s)
	until := time.Now().Add(rotateCooldown)
	s.mu.Lock()
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.mu.Unlock()
	logger(ctx).InfoContext(ctx, "rotated proxy session", "session", s.name, "reason", reason)
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
