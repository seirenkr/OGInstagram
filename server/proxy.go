package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

	budgetLeaseExpires   time.Time
	budgetLeaseRemaining int64
	budgetExhaustedUntil time.Time
	budgetBackendRetryAt time.Time
}

type proxyBudgetError struct {
	code string
}

func (e *proxyBudgetError) Error() string {
	if e.code == errorCodeBudgetExhausted {
		return "daily proxy bandwidth budget reached"
	}
	return "proxy bandwidth budget is temporarily unavailable"
}

func proxyBudgetErrorCode(err error) string {
	var budgetErr *proxyBudgetError
	if errors.As(err, &budgetErr) {
		return budgetErr.code
	}
	return ""
}

type budgetedConn struct {
	net.Conn
	pool *SessionPool
}

func (c *budgetedConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return c.Conn.Read(p)
	}
	if code := c.pool.reserveProxyBytes(int64(len(p))); code != "" {
		_ = c.Conn.Close()
		return 0, &proxyBudgetError{code: code}
	}
	n, err := c.Conn.Read(p)
	c.pool.refundProxyBytes(int64(len(p) - n))
	return n, err
}

func (c *budgetedConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return c.Conn.Write(p)
	}
	if code := c.pool.reserveProxyBytes(int64(len(p))); code != "" {
		_ = c.Conn.Close()
		return 0, &proxyBudgetError{code: code}
	}
	n, err := c.Conn.Write(p)
	c.pool.refundProxyBytes(int64(len(p) - n))
	return n, err
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{
		cfg:    cfg,
		budget: &http.Client{Timeout: budgetRequestTimeout},
	}
	now := time.Now()
	add := func(s *Session) {
		client, err := buildSessionClient(s.proxyURL, pool)
		if err != nil {
			slog.Warn("proxy session skipped", "event", "proxy_session_skipped", "session", s.name,
				"error.type", "invalid_proxy_configuration", "exception.message", err.Error())
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

func buildSessionClient(proxyURL string, pool *SessionPool) (*http.Client, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: http.ProxyURL(parsed),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &budgetedConn{Conn: conn, pool: pool}, nil
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport}, nil
}

func (p *SessionPool) pick(ctx context.Context) (*Session, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return nil, errorCodeConnection
	}
	now := time.Now()

	var picked *Session
	bestRank := 0.0
	for _, s := range p.sessions {
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
		if errorCode := p.ensureBudgetBytesLocked(ctx, now, 1); errorCode != "" {
			return nil, errorCode
		}
		if ctx.Err() != nil {
			return nil, errorCodeConnection
		}
		picked.mu.Lock()
		picked.used++
		picked.mu.Unlock()
	}
	return picked, ""
}

func (p *SessionPool) ensureBudgetBytesLocked(ctx context.Context, now time.Time, required int64) string {
	if required <= 0 {
		return ""
	}
	if !now.Before(p.budgetLeaseExpires) {
		p.budgetLeaseRemaining = 0
		p.budgetLeaseExpires = time.Time{}
	}
	if p.budgetLeaseRemaining >= required {
		return ""
	}
	if now.Before(p.budgetExhaustedUntil) {
		return errorCodeBudgetExhausted
	}
	p.budgetExhaustedUntil = time.Time{}
	if now.Before(p.budgetBackendRetryAt) {
		return errorCodeBudgetBackend
	}
	p.budgetBackendRetryAt = time.Time{}

	for p.budgetLeaseRemaining < required {
		requestCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetRequestTimeout)
		req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, p.cfg.BudgetURL, nil)
		if err != nil {
			cancel()
			p.logBudgetFailure(ctx, "request", 0, err)
			p.memoizeBudgetBackendFailureLocked()
			return errorCodeBudgetBackend
		}
		resp, err := p.budget.Do(req)
		cancel()
		if err != nil {
			p.logBudgetFailure(ctx, "connection", 0, err)
			p.memoizeBudgetBackendFailureLocked()
			return errorCodeBudgetBackend
		}
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusNoContent:
			granted, grantErr := strconv.ParseInt(resp.Header.Get(headerProxyBudgetGrantedBytes), 10, 64)
			expiresAt, expiryErr := strconv.ParseInt(resp.Header.Get(headerProxyBudgetExpires), 10, 64)
			if grantErr != nil || expiryErr != nil || granted < 1 || granted > proxyByteLeaseSize ||
				expiresAt <= time.Now().UnixMilli() {
				p.logBudgetFailure(ctx, "protocol", resp.StatusCode, nil)
				p.memoizeBudgetBackendFailureLocked()
				return errorCodeBudgetBackend
			}
			expires := time.UnixMilli(expiresAt)
			if !p.budgetLeaseExpires.IsZero() && !p.budgetLeaseExpires.Equal(expires) {
				p.budgetLeaseRemaining = 0
			}
			p.budgetLeaseRemaining += granted
			p.budgetLeaseExpires = expires
		case http.StatusTooManyRequests:
			expiresAt, err := strconv.ParseInt(resp.Header.Get(headerProxyBudgetExpires), 10, 64)
			if err != nil || expiresAt <= time.Now().UnixMilli() {
				p.logBudgetFailure(ctx, "protocol", resp.StatusCode, err)
				p.memoizeBudgetBackendFailureLocked()
				return errorCodeBudgetBackend
			}
			p.budgetExhaustedUntil = time.UnixMilli(expiresAt)
			return errorCodeBudgetExhausted
		default:
			p.logBudgetFailure(ctx, "status", resp.StatusCode, nil)
			p.memoizeBudgetBackendFailureLocked()
			return errorCodeBudgetBackend
		}
	}
	return ""
}

func (p *SessionPool) reserveProxyBytes(bytes int64) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if code := p.ensureBudgetBytesLocked(context.Background(), time.Now(), bytes); code != "" {
		return code
	}
	p.budgetLeaseRemaining -= bytes
	return ""
}

func (p *SessionPool) refundProxyBytes(bytes int64) {
	if bytes <= 0 {
		return
	}
	p.mu.Lock()
	if time.Now().Before(p.budgetLeaseExpires) {
		p.budgetLeaseRemaining += bytes
	}
	p.mu.Unlock()
}

func (p *SessionPool) memoizeBudgetBackendFailureLocked() {
	p.budgetBackendRetryAt = time.Now().Add(budgetBackendRetryDelay)
}

func (p *SessionPool) logBudgetFailure(ctx context.Context, failure string, status int, err error) {
	attrs := []any{
		"event", "proxy_budget_failed",
		"error.type", "budget_" + failure,
	}
	if status != 0 {
		attrs = append(attrs, "http.response.status_code", status)
	}
	if err != nil {
		attrs = append(attrs, "exception.message", err.Error())
	}
	logger(ctx).ErrorContext(ctx, "proxy budget request failed", attrs...)
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

func (p *SessionPool) fail(s *Session) {
	p.rotate(s)
	until := time.Now().Add(rotateCooldown)
	s.mu.Lock()
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.mu.Unlock()
}

func (p *SessionPool) rotate(s *Session) {
	s.mu.Lock()
	if p.cfg.ProxyUser != "" {
		s.sessionID = newSessionID()
		s.proxyURL = proxyURL(p.cfg.ProxyUser, p.cfg.ProxyPass, s.sessionID)
	}
	proxyURL := s.proxyURL
	s.mu.Unlock()

	client, err := buildSessionClient(proxyURL, p)
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
