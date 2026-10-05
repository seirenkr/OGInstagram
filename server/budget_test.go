package main

import (
	"context"
	"testing"
	"time"
)

func TestProxyByteRefundDoesNotCrossLeaseBoundary(t *testing.T) {
	oldExpiry := time.Now().Add(time.Hour)
	pool := &SessionPool{
		budgetLeaseExpires:   oldExpiry,
		budgetLeaseRemaining: 8,
	}
	leaseExpires, reason := pool.reserveProxyBytes(8)
	if reason != "" {
		t.Fatalf("reservation denied with %q", reason)
	}

	pool.mu.Lock()
	pool.budgetLeaseExpires = oldExpiry.Add(24 * time.Hour)
	pool.budgetLeaseRemaining = 7
	pool.mu.Unlock()
	pool.refundProxyBytes(8, leaseExpires)

	if pool.budgetLeaseRemaining != 7 {
		t.Fatalf("old lease refund changed new lease to %d bytes, want 7", pool.budgetLeaseRemaining)
	}
}

func TestProxySessionHourlyRequestLimit(t *testing.T) {
	now := time.Now()
	session := &Session{windowStart: now}
	pool := &SessionPool{
		budgetLeaseExpires:   now.Add(24 * time.Hour),
		budgetLeaseRemaining: proxyByteLeaseSize,
		sessions:             []*Session{session},
	}
	for i := 0; i < defaultProxyHourlyLimit; i++ {
		if picked, reason := pool.pick(context.Background()); picked != session || reason != "" {
			t.Fatalf("pick %d = (%v, %q), want session", i+1, picked, reason)
		}
	}
	if picked, reason := pool.pick(context.Background()); picked != nil || reason != "" {
		t.Fatalf("pick beyond hourly session limit = (%v, %q), want unavailable", picked, reason)
	}

	session.mu.Lock()
	session.windowStart = now.Add(-time.Hour)
	session.mu.Unlock()
	if picked, reason := pool.pick(context.Background()); picked != session || reason != "" {
		t.Fatalf("pick after session window reset = (%v, %q), want session", picked, reason)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.used != 1 {
		t.Fatalf("session usage after reset = %d, want 1", session.used)
	}
}

func TestProxyPickDoesNotReserveAfterWaitingContextIsCancelled(t *testing.T) {
	pool := &SessionPool{
		budgetLeaseExpires:   time.Now().Add(24 * time.Hour),
		budgetLeaseRemaining: 1,
		sessions:             []*Session{{windowStart: time.Now()}},
	}
	pool.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan string, 1)
	go func() {
		session, reason := pool.pick(ctx)
		if session != nil {
			result <- "reserved"
			return
		}
		result <- reason
	}()
	cancel()
	pool.mu.Unlock()

	if reason := <-result; reason != errorCodeConnection {
		t.Fatalf("cancelled pick reason = %q, want %q", reason, errorCodeConnection)
	}
	if pool.budgetLeaseRemaining != 1 {
		t.Fatalf("cancelled pick changed lease to %d bytes", pool.budgetLeaseRemaining)
	}
	if got := pool.sessions[0].used; got != 0 {
		t.Fatalf("cancelled pick consumed %d session requests", got)
	}
}

func TestExpiredProxyLeaseLeftoverIsDiscarded(t *testing.T) {
	p := newBudgetTestPool(t, 0)
	p.budgetLeaseExpires = time.Now().Add(-time.Second)
	p.budgetLeaseRemaining = proxyByteLeaseSize
	if _, reason := p.reserveProxyBytes(1); reason != errorCodeBudgetExhausted {
		t.Fatalf("expired lease spent: %q", reason)
	}
}
