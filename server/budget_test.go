package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type durableBudgetFake struct {
	mu    sync.Mutex
	limit int64
	used  int64
	calls int
}

func (b *durableBudgetFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	expiresAt := time.Now().Add(24 * time.Hour).UnixMilli()
	w.Header().Set(headerProxyBudgetExpires, strconv.FormatInt(expiresAt, 10))
	granted := min(proxyByteLeaseSize, max(int64(0), b.limit-b.used))
	if granted == 0 {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	b.used += granted
	w.Header().Set(headerProxyBudgetGrantedBytes, strconv.FormatInt(granted, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (b *durableBudgetFake) state() (used int64, calls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.calls
}

func newBudgetTestPool(server *httptest.Server) *SessionPool {
	return &SessionPool{
		cfg:      Config{BudgetURL: server.URL},
		budget:   server.Client(),
		sessions: []*Session{{windowStart: time.Now()}},
	}
}

func TestProxyByteLeaseAmortizesReservations(t *testing.T) {
	backend := &durableBudgetFake{limit: 2 * proxyByteLeaseSize}
	server := httptest.NewServer(backend)
	defer server.Close()
	pool := newBudgetTestPool(server)

	const reservation = proxyByteLeaseSize / 4
	for i := 0; i < 4; i++ {
		if reason := pool.reserveProxyBytes(reservation); reason != "" {
			t.Fatalf("reservation %d denied with %q", i+1, reason)
		}
	}
	if used, calls := backend.state(); used != proxyByteLeaseSize || calls != 1 {
		t.Fatalf("after one lease: used=%d calls=%d, want %d and 1",
			used, calls, proxyByteLeaseSize)
	}

	if reason := pool.reserveProxyBytes(1); reason != "" {
		t.Fatalf("first byte in second lease denied with %q", reason)
	}
	if used, calls := backend.state(); used != 2*proxyByteLeaseSize || calls != 2 {
		t.Fatalf("after second lease: used=%d calls=%d, want %d and 2",
			used, calls, 2*proxyByteLeaseSize)
	}
}

func TestConcurrentProxyBytesShareOneLease(t *testing.T) {
	backend := &durableBudgetFake{limit: proxyByteLeaseSize}
	server := httptest.NewServer(backend)
	defer server.Close()
	pool := newBudgetTestPool(server)

	const callers = 32
	const reservation = proxyByteLeaseSize / callers
	var wg sync.WaitGroup
	failures := make(chan string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reason := pool.reserveProxyBytes(reservation); reason != "" {
				failures <- reason
			}
		}()
	}
	wg.Wait()
	close(failures)
	for reason := range failures {
		t.Errorf("concurrent pick was denied with %q", reason)
	}
	if used, calls := backend.state(); used != proxyByteLeaseSize || calls != 1 {
		t.Fatalf("concurrent lease state: used=%d calls=%d, want %d and 1",
			used, calls, proxyByteLeaseSize)
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

func TestProxyBudgetExhaustionIsFailClosedAndMemoized(t *testing.T) {
	backend := &durableBudgetFake{limit: 2}
	server := httptest.NewServer(backend)
	defer server.Close()
	pool := newBudgetTestPool(server)

	for i := 0; i < 2; i++ {
		if reason := pool.reserveProxyBytes(1); reason != "" {
			t.Fatalf("allowed byte %d denied with %q", i+1, reason)
		}
	}
	if reason := pool.reserveProxyBytes(1); reason != errorCodeBudgetExhausted {
		t.Fatalf("exhausted reservation = %q, want %q", reason, errorCodeBudgetExhausted)
	}

	if reason := pool.reserveProxyBytes(1); reason != errorCodeBudgetExhausted {
		t.Fatalf("memoized exhausted reservation = %q, want %q", reason, errorCodeBudgetExhausted)
	}
	if used, calls := backend.state(); used != 2 || calls != 2 {
		t.Fatalf("exhausted backend state: used=%d calls=%d, want 2 and 2", used, calls)
	}
}

func TestProxyBudgetSurvivesSessionPoolRestart(t *testing.T) {
	backend := &durableBudgetFake{limit: proxyByteLeaseSize + 5}
	server := httptest.NewServer(backend)
	defer server.Close()

	firstProcess := newBudgetTestPool(server)
	if reason := firstProcess.reserveProxyBytes(proxyByteLeaseSize); reason != "" {
		t.Fatalf("first process reservation denied with %q", reason)
	}

	secondProcess := newBudgetTestPool(server)
	if reason := secondProcess.reserveProxyBytes(5); reason != "" {
		t.Fatalf("second process reservation denied with %q", reason)
	}
	if reason := secondProcess.reserveProxyBytes(1); reason != errorCodeBudgetExhausted {
		t.Fatalf("post-restart reservation = %q, want %q", reason, errorCodeBudgetExhausted)
	}
	if used, _ := backend.state(); used != proxyByteLeaseSize+5 {
		t.Fatalf("durable usage after restart = %d, want %d", used, proxyByteLeaseSize+5)
	}
}

func TestBudgetedConnChargesEveryReadAndWriteByte(t *testing.T) {
	backend := &durableBudgetFake{limit: proxyByteLeaseSize}
	server := httptest.NewServer(backend)
	defer server.Close()
	pool := newBudgetTestPool(server)

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn := &budgetedConn{Conn: local, pool: pool}

	written := []byte("CONNECT and TLS bytes")
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, len(written))
		_, err := io.ReadFull(remote, buf)
		readDone <- err
	}()
	if n, err := conn.Write(written); err != nil || n != len(written) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", n, err, len(written))
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}

	received := []byte("response bytes")
	go func() {
		_, _ = remote.Write(received)
	}()
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != string(received) {
		t.Fatalf("Read() = (%q, %v), want (%q, nil)", buf[:n], err, received)
	}

	wantRemaining := proxyByteLeaseSize - int64(len(written)+len(received))
	if pool.budgetLeaseRemaining != wantRemaining {
		t.Fatalf("remaining byte lease = %d, want %d", pool.budgetLeaseRemaining, wantRemaining)
	}
	if used, calls := backend.state(); used != proxyByteLeaseSize || calls != 1 {
		t.Fatalf("durable lease = (%d bytes, %d calls), want (%d, 1)", used, calls, proxyByteLeaseSize)
	}
}

func TestBudgetedConnFailsClosedBeforeUnbudgetedWrite(t *testing.T) {
	backend := &durableBudgetFake{limit: 2}
	server := httptest.NewServer(backend)
	defer server.Close()
	pool := newBudgetTestPool(server)

	local, remote := net.Pipe()
	defer remote.Close()
	conn := &budgetedConn{Conn: local, pool: pool}
	n, err := conn.Write([]byte("abc"))
	if n != 0 || proxyBudgetErrorCode(err) != errorCodeBudgetExhausted {
		t.Fatalf("Write() = (%d, %v), want budget exhaustion", n, err)
	}
}

func TestProxyBudgetBackendFailuresAreFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable},
		{
			name:   "malformed lease",
			status: http.StatusNoContent,
			headers: map[string]string{
				headerProxyBudgetGrantedBytes: "not-a-number",
				headerProxyBudgetExpires:      strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			pool := newBudgetTestPool(server)

			session, reason := pool.pick(context.Background())
			if session != nil || reason != errorCodeBudgetBackend {
				t.Fatalf("backend failure = (%v, %q), want %q", session, reason, errorCodeBudgetBackend)
			}
			if got := pool.sessions[0].used; got != 0 {
				t.Fatalf("failed byte-budget reservation consumed %d session requests", got)
			}
		})
	}
}

func TestConcurrentBudgetBackendFailureIsMemoized(t *testing.T) {
	var calls int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	pool := newBudgetTestPool(server)

	const callers = 32
	var wg sync.WaitGroup
	failures := make(chan string, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, reason := pool.pick(context.Background())
			if session != nil || reason != errorCodeBudgetBackend {
				failures <- reason
			}
		}()
	}
	wg.Wait()
	close(failures)
	for reason := range failures {
		t.Errorf("backend failure returned unexpected reason %q", reason)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("concurrent backend failures made %d requests, want 1", calls)
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

func TestBudgetLeaseOutlivesCancelledLeaderWithoutChargingIt(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header: http.Header{
				headerProxyBudgetGrantedBytes: {strconv.FormatInt(proxyByteLeaseSize, 10)},
				headerProxyBudgetExpires:      {strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10)},
			},
			Body: http.NoBody,
		}, nil
	})}
	pool := &SessionPool{
		cfg:      Config{BudgetURL: "http://budget.internal/"},
		budget:   client,
		sessions: []*Session{{windowStart: time.Now()}},
	}
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
	<-started
	cancel()
	close(release)
	if reason := <-result; reason != errorCodeConnection {
		t.Fatalf("cancelled leader reason = %q, want %q", reason, errorCodeConnection)
	}
	if pool.budgetLeaseRemaining != proxyByteLeaseSize {
		t.Fatalf("cancelled leader changed lease to %d bytes", pool.budgetLeaseRemaining)
	}
	if got := pool.sessions[0].used; got != 0 {
		t.Fatalf("cancelled leader consumed %d session requests", got)
	}

	if session, reason := pool.pick(context.Background()); session == nil || reason != "" {
		t.Fatalf("live waiter could not use shared lease: session=%v reason=%q", session, reason)
	}
	if calls.Load() != 1 || pool.budgetLeaseRemaining != proxyByteLeaseSize {
		t.Fatalf("shared lease calls=%d remaining=%d, want 1 and %d",
			calls.Load(), pool.budgetLeaseRemaining, proxyByteLeaseSize)
	}
	if got := pool.sessions[0].used; got != 1 {
		t.Fatalf("live waiter consumed %d session requests, want 1", got)
	}
}

func TestRaceFetchReportsDurableBudgetReason(t *testing.T) {
	for _, tt := range []struct {
		status int
		reason string
	}{
		{status: http.StatusTooManyRequests, reason: errorCodeBudgetExhausted},
		{status: http.StatusServiceUnavailable, reason: errorCodeBudgetBackend},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status == http.StatusTooManyRequests {
					w.Header().Set(headerProxyBudgetExpires, strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10))
				}
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			pool := newBudgetTestPool(server)
			app := &App{pool: pool}

			_, _, err := app.fetchViaProxy(context.Background(), fetchSpec{})
			if err == nil || err.Code != tt.reason || !err.Ephemeral {
				t.Fatalf("race budget error = %+v, want ephemeral %q", err, tt.reason)
			}
			if got := pool.sessions[0].used; got != 0 {
				t.Fatalf("failed byte-budget reservation consumed %d session requests", got)
			}
		})
	}
}
