package main

import (
	"context"
	"crypto/tls"
	"errors"
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

	cooldownUntil       time.Time
	helperCooldownUntil time.Time
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
	pool := &SessionPool{cfg: cfg, tlsCache: tls.NewLRUClientSessionCache(proxySessionCount * 2)}
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
		DisableCompression:  true,
		TLSHandshakeTimeout: 3 * time.Second,
		TLSClientConfig:     &tls.Config{ClientSessionCache: pool.tlsCache},
		// Close a silent tunnel instead of letting requests hang on it.
		HTTP2: &http.HTTP2Config{
			MaxDecoderHeaderTableSize: 64 << 10,
			// Bound bytes already in flight when an unexpected HTML response is rejected.
			MaxReceiveBufferPerStream: 32 << 10,
			SendPingTimeout:           5 * time.Second,
			PingTimeout:               2 * time.Second,
		},
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport}, nil
}

func (p *SessionPool) pick(ctx context.Context, helper bool) (*Session, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return nil, errorCodeConnection
	}
	now := time.Now()

	ready := make([]*Session, 0, len(p.sessions))
	pickReason := ""
	for _, s := range p.sessions {
		s.mu.Lock()
		if !s.cooldownUntil.After(now) {
			if helper && s.helperCooldownUntil.After(now) {
				pickReason = errorCodeCaptchaRequired
			} else {
				ready = append(ready, s)
			}
		}
		s.mu.Unlock()
	}
	var picked *Session
	switch len(ready) {
	case 0:
		return nil, pickReason
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

	if errorCode := p.ensureBudgetBytesLocked(ctx, now, 1); errorCode != "" {
		return nil, errorCode
	}
	if ctx.Err() != nil {
		return nil, errorCodeConnection
	}
	picked.mu.Lock()
	picked.pending++
	picked.mu.Unlock()
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

// done ends a request picked from s. With observe, rtt feeds the Peak-EWMA
// estimate; tower's update rule, weighted by the time since the last sample.
func (p *SessionPool) done(s *Session, rtt time.Duration, observe bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = max(s.pending-1, 0)
	if !observe {
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
	s.rttNs, s.rttAt = 0, time.Time{} // a new exit IP starts unmeasured
	s.helperCooldownUntil = time.Time{}
	s.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}
