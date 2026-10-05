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

	mu     sync.Mutex
	client *http.Client

	ewmaMs        float64
	hasEWMA       bool
	cooldownUntil time.Time
}

type SessionPool struct {
	sessions []*Session
	cfg      Config
	mu       sync.Mutex

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
	return "daily proxy bandwidth budget is temporarily unavailable"
}

func budgetAppError(code string) *AppError {
	return ephemeralErr(503, code, (&proxyBudgetError{code}).Error())
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

func (c *budgetedConn) Read(p []byte) (int, error)  { return c.metered(p, c.Conn.Read) }
func (c *budgetedConn) Write(p []byte) (int, error) { return c.metered(p, c.Conn.Write) }

// Reserve len(p) before the transfer and refund what it did not move.
func (c *budgetedConn) metered(p []byte, op func([]byte) (int, error)) (int, error) {
	if len(p) == 0 {
		return op(p)
	}
	leaseExpires, code := c.pool.reserveProxyBytes(int64(len(p)))
	if code != "" {
		_ = c.Conn.Close()
		return 0, &proxyBudgetError{code: code}
	}
	n, err := op(p)
	c.pool.refundProxyBytes(int64(len(p)-n), leaseExpires)
	return n, err
}

func newSessionPool(cfg Config) *SessionPool {
	pool := &SessionPool{cfg: cfg}
	if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
		for i := range proxySessionCount {
			// A stable slot label: rotation replaces the exit IP behind it.
			name := "us-" + strconv.Itoa(i+1)
			client, err := buildSessionClient(proxyURL(cfg.ProxyUser, cfg.ProxyPass, newSessionID()), pool)
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
	transport := &http.Transport{
		Proxy: http.ProxyURL(parsed),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &budgetedConn{Conn: conn, pool: pool}, nil
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 3 * time.Second,
		// Close a silent tunnel instead of letting requests hang on it.
		HTTP2:                 &http.HTTP2Config{SendPingTimeout: 5 * time.Second, PingTimeout: 2 * time.Second},
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
		ok := !s.cooldownUntil.After(now)
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
	}
	return picked, ""
}

func (p *SessionPool) ensureBudgetBytesLocked(ctx context.Context, now time.Time, required int64) string {
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
		granted, expires, err := p.cfg.Store.takeBudget(requestCtx, time.Now())
		cancel()
		if err != nil {
			logger(ctx).ErrorContext(ctx, "proxy budget request failed", "error_type", "budget_storage", "error", err)
			p.budgetBackendRetryAt = time.Now().Add(budgetBackendRetryDelay)
			return errorCodeBudgetBackend
		}
		if granted == 0 {
			p.budgetExhaustedUntil = expires
			return errorCodeBudgetExhausted
		}
		// A grant can finish across UTC midnight while waiting for SQLite. Its
		// old-day charge remains durable, but only a live lease may send bytes.
		if !expires.After(time.Now()) {
			p.budgetLeaseRemaining = 0
			p.budgetLeaseExpires = time.Time{}
			continue
		}
		if !p.budgetLeaseExpires.IsZero() && !p.budgetLeaseExpires.Equal(expires) {
			p.budgetLeaseRemaining = 0
		}
		p.budgetLeaseRemaining += granted
		p.budgetLeaseExpires = expires
	}
	return ""
}

func (p *SessionPool) reserveProxyBytes(bytes int64) (time.Time, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if code := p.ensureBudgetBytesLocked(context.Background(), time.Now(), bytes); code != "" {
		return time.Time{}, code
	}
	p.budgetLeaseRemaining -= bytes
	return p.budgetLeaseExpires, ""
}

func (p *SessionPool) refundProxyBytes(bytes int64, leaseExpires time.Time) {
	if bytes <= 0 {
		return
	}
	p.mu.Lock()
	if p.budgetLeaseExpires.Equal(leaseExpires) && time.Now().Before(leaseExpires) {
		p.budgetLeaseRemaining += bytes
	}
	p.mu.Unlock()
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
	client, err := buildSessionClient(proxyURL(p.cfg.ProxyUser, p.cfg.ProxyPass, newSessionID()), p)
	if err != nil {
		slog.Warn("proxy session rotation failed", "session", s.name, "error", err)
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
