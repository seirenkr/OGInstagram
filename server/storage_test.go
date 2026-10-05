package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *localStore {
	t.Helper()
	s, err := openLocalStore(t.TempDir(), time.Now().UTC().Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newBudgetTestPool(t *testing.T, allowance int64) *SessionPool {
	t.Helper()
	s := newTestStore(t)
	now := time.Now().UTC()
	_, err := s.budget.Exec("INSERT INTO budget_days(day, used) VALUES (?, ?)", now.Format(time.DateOnly), proxyDailyByteBudget(now)-allowance)
	if err != nil {
		t.Fatal(err)
	}
	return &SessionPool{cfg: Config{Store: s}, sessions: []*Session{{}}}
}

func remainingDailyBudget(t *testing.T, s *localStore) int64 {
	t.Helper()
	now := time.Now().UTC()
	var used int64
	err := s.budget.QueryRow("SELECT used FROM budget_days WHERE day = ?", now.Format(time.DateOnly)).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return proxyDailyByteBudget(now)
	}
	if err != nil {
		t.Fatal(err)
	}
	return proxyDailyByteBudget(now) - used
}

func TestLocalBudgetDailyPolicyAndStartDate(t *testing.T) {
	for _, tc := range []struct {
		date string
		days int64
	}{{"2026-01-15", 31}, {"2026-02-15", 28}, {"2028-02-15", 29}, {"2026-04-15", 30}} {
		date, _ := time.Parse(time.DateOnly, tc.date)
		if got := proxyDailyByteBudget(date); got != proxyMonthlyBudgetBytes/tc.days {
			t.Errorf("%s budget=%d", tc.date, got)
		}
	}
	if _, err := openLocalStore(t.TempDir(), ""); err == nil {
		t.Fatal("missing mandatory budget date accepted")
	}
	s, err := openLocalStore(t.TempDir(), "2026-10-06")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	day, _ := time.Parse(time.DateOnly, "2026-10-05")
	grant, expires, err := s.takeBudget(context.Background(), day)
	if err != nil || grant != 0 || expires.Format(time.DateOnly) != "2026-10-06" {
		t.Fatalf("pre-start grant=(%d,%s,%v)", grant, expires, err)
	}
}

func TestLocalBudgetLeasesAreAtomicAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().UTC().Format(time.DateOnly)
	first, err := openLocalStore(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openLocalStore(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	const allowance = 3*proxyByteLeaseSize + 7
	_, err = first.budget.Exec("INSERT INTO budget_days(day,used) VALUES (?,?)", day, proxyDailyByteBudget(time.Now())-allowance)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var total atomic.Int64
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := first
			if i%2 == 1 {
				store = second
			}
			grant, _, err := store.takeBudget(context.Background(), time.Now())
			if err != nil {
				failures <- err
			}
			total.Add(grant)
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if total.Load() != allowance || remainingDailyBudget(t, first) != 0 {
		t.Fatalf("atomic grant total=%d, allowance=%d", total.Load(), allowance)
	}
}

func TestLocalBudgetLeaseDurabilityAndMonotonicStart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	s, err := openLocalStore(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	first := &SessionPool{cfg: Config{Store: s}}
	if _, reason := first.reserveProxyBytes(1); reason != "" {
		t.Fatal(reason)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openLocalStore(dir, now.AddDate(0, 0, -1).Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second := &SessionPool{cfg: Config{Store: s}}
	if _, reason := second.reserveProxyBytes(1); reason != "" {
		t.Fatal(reason)
	}
	if got := remainingDailyBudget(t, s); got != proxyDailyByteBudget(now)-2*proxyByteLeaseSize {
		t.Fatalf("restart regained unused lease: remaining=%d", got)
	}
	grant, _, err := s.takeBudget(context.Background(), now.AddDate(0, 0, -1))
	if err != nil || grant != 0 {
		t.Fatalf("lower date reopened budget: %d,%v", grant, err)
	}
	var syncMode int
	var journal string
	if err = s.budget.QueryRow("PRAGMA synchronous").Scan(&syncMode); err != nil {
		t.Fatal(err)
	}
	if err = s.budget.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if syncMode != 2 || journal != "wal" {
		t.Fatalf("ledger durability synchronous=%d journal=%s", syncMode, journal)
	}
}

func TestLostLocalBudgetLedgerCannotRegainTodaysAllowance(t *testing.T) {
	for _, damage := range []string{"missing file", "zero file", "truncated file", "missing metadata table", "missing metadata row", "missing usage table", "missing last usage row"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			day := time.Now().UTC().Format(time.DateOnly)
			s, err := openLocalStore(dir, day)
			if err != nil {
				t.Fatal(err)
			}
			if grant, _, err := s.takeBudget(context.Background(), time.Now()); err != nil || grant != proxyByteLeaseSize {
				t.Fatalf("initial charged lease: %d,%v", grant, err)
			}
			switch damage {
			case "missing metadata table":
				_, err = s.budget.Exec("DROP TABLE budget_config")
			case "missing metadata row":
				_, err = s.budget.Exec("DELETE FROM budget_config")
			case "missing usage table":
				_, err = s.budget.Exec("DROP TABLE budget_days")
			case "missing last usage row":
				_, err = s.budget.Exec("DELETE FROM budget_days WHERE day = ?", day)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			ledger := filepath.Join(dir, "budget.sqlite")
			switch damage {
			case "missing file":
				err = os.Remove(ledger)
			case "zero file":
				err = os.Truncate(ledger, 0)
			case "truncated file":
				err = os.Truncate(ledger, 100)
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err = openLocalStore(dir, day)
			if damage == "truncated file" && err != nil {
				return
			} // Invalid SQLite is rejected at startup.
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if grant, _, err := s.takeBudget(context.Background(), time.Now()); err != nil || grant != 0 {
				t.Fatalf("lost ledger regained allowance: %d,%v", grant, err)
			}
		})
	}
}

func TestLocalBudgetDeniesClockRollbackAndDoesNotCarryUnusedBytes(t *testing.T) {
	s, err := openLocalStore(t.TempDir(), "2026-10-01")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, _ := time.Parse(time.DateOnly, "2026-10-05")
	second := first.AddDate(0, 0, 1)
	for _, day := range []time.Time{first, second} {
		if grant, _, err := s.takeBudget(context.Background(), day); err != nil || grant != proxyByteLeaseSize {
			t.Fatalf("new UTC day grant=%d,%v", grant, err)
		}
	}
	if grant, _, err := s.takeBudget(context.Background(), first); err != nil || grant != 0 {
		t.Fatalf("clock rollback reopened allowance: %d,%v", grant, err)
	}
	var used int64
	if err := s.budget.QueryRow("SELECT used FROM budget_days WHERE day = ?", second.Format(time.DateOnly)).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != proxyByteLeaseSize {
		t.Fatal("unused previous-day bytes carried into the next day")
	}
}

func TestProxyByteLeaseAmortizesReservations(t *testing.T) {
	p := newBudgetTestPool(t, 2*proxyByteLeaseSize)
	for range 4 {
		if _, reason := p.reserveProxyBytes(proxyByteLeaseSize / 4); reason != "" {
			t.Fatal(reason)
		}
	}
	if remainingDailyBudget(t, p.cfg.Store) != proxyByteLeaseSize {
		t.Fatal("reservations did not share one lease")
	}
	if _, reason := p.reserveProxyBytes(1); reason != "" {
		t.Fatal(reason)
	}
	if remainingDailyBudget(t, p.cfg.Store) != 0 {
		t.Fatal("second lease was not charged")
	}
}

func TestProxyBudgetExhaustionAndBackendFailuresFailClosed(t *testing.T) {
	p := newBudgetTestPool(t, 2)
	for range 2 {
		if _, reason := p.reserveProxyBytes(1); reason != "" {
			t.Fatal(reason)
		}
	}
	if _, reason := p.reserveProxyBytes(1); reason != errorCodeBudgetExhausted {
		t.Fatalf("exhaustion reason=%s", reason)
	}
	if _, reason := p.reserveProxyBytes(1); reason != errorCodeBudgetExhausted {
		t.Fatalf("memoized exhaustion reason=%s", reason)
	}
	if session, reason := p.pick(context.Background()); session != nil || reason != errorCodeBudgetExhausted {
		t.Fatalf("exhausted budget pick=%v,%s", session, reason)
	}
	for _, store := range []*localStore{nil, newTestStore(t)} {
		if store != nil {
			_ = store.budget.Close()
		}
		p := &SessionPool{cfg: Config{Store: store}, sessions: []*Session{{}}}
		if session, reason := p.pick(context.Background()); session != nil || reason != errorCodeBudgetBackend {
			t.Fatalf("backend failure=%v,%s", session, reason)
		}
		if p.budgetBackendRetryAt.IsZero() {
			t.Fatal("failed budget was not memoized")
		}
		p.cfg.Store = newTestStore(t)
		if _, reason := p.reserveProxyBytes(1); reason != errorCodeBudgetBackend {
			t.Fatalf("memo bypassed: %q", reason)
		}
		p.budgetBackendRetryAt = time.Now().Add(-time.Second)
		if _, reason := p.reserveProxyBytes(1); reason != "" {
			t.Fatalf("no retry after memo: %q", reason)
		}
	}
}

func TestConcurrentProxyBytesShareOneLease(t *testing.T) {
	p := newBudgetTestPool(t, proxyByteLeaseSize)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, reason := p.reserveProxyBytes(proxyByteLeaseSize / 32); reason != "" {
				t.Error(reason)
			}
		}()
	}
	wg.Wait()
	if remainingDailyBudget(t, p.cfg.Store) != 0 || p.budgetLeaseRemaining != 0 {
		t.Fatal("concurrent reservations did not share their lease")
	}
}

func TestBudgetedConnChargesEveryReadAndWriteByte(t *testing.T) {
	p := newBudgetTestPool(t, proxyByteLeaseSize)
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	conn := &budgetedConn{Conn: local, pool: p}
	written := []byte("CONNECT and TLS bytes")
	readDone := make(chan error, 1)
	go func() { buf := make([]byte, len(written)); _, err := io.ReadFull(remote, buf); readDone <- err }()
	if n, err := conn.Write(written); err != nil || n != len(written) {
		t.Fatalf("Write=%d,%v", n, err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	received := []byte("response bytes")
	go func() { _, _ = remote.Write(received) }()
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != string(received) {
		t.Fatalf("Read=%q,%v", buf[:n], err)
	}
	if p.budgetLeaseRemaining != proxyByteLeaseSize-int64(len(written)+len(received)) {
		t.Fatal("read/write transport bytes not charged")
	}
}

func TestBudgetedConnFailsClosedBeforeUnbudgetedWrite(t *testing.T) {
	p := newBudgetTestPool(t, 2)
	local, remote := net.Pipe()
	defer remote.Close()
	conn := &budgetedConn{Conn: local, pool: p}
	if n, err := conn.Write([]byte("abc")); n != 0 || proxyBudgetErrorCode(err) != errorCodeBudgetExhausted {
		t.Fatalf("Write=%d,%v", n, err)
	}
}

func TestBudgetLeaseOutlivesCancelledLeaderWithoutChargingIt(t *testing.T) {
	s := newTestStore(t)
	// Occupy the sole connection, then release it after the leader cancels. Its
	// durable grant completes for future callers without selecting a session.
	connection, err := s.budget.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := &SessionPool{cfg: Config{Store: s}, sessions: []*Session{{}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		session, reason := p.pick(ctx)
		if session != nil {
			reason = "reserved"
		}
		done <- reason
	}()
	waitUntil(t, time.Second, func() bool { return s.budget.Stats().WaitCount > 0 })
	cancel()
	_ = connection.Close()
	if reason := <-done; reason != errorCodeConnection {
		t.Fatalf("cancelled leader=%s", reason)
	}
	if p.budgetLeaseRemaining != proxyByteLeaseSize {
		t.Fatal("cancelled leader consumed shared grant")
	}
	if session, reason := p.pick(context.Background()); session == nil || reason != "" {
		t.Fatalf("live waiter=%v,%s", session, reason)
	}
	if remainingDailyBudget(t, s) != proxyDailyByteBudget(time.Now())-proxyByteLeaseSize {
		t.Fatal("waiter requested another lease")
	}
}

func TestPersistentCacheRestartExpiryAndValidation(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().UTC().Format(time.DateOnly)
	s, err := openLocalStore(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	c := newPersistentCache[int](s, "post", make(chan struct{}, 1), 4096)
	var fetches atomic.Int32
	fetch := func(context.Context) (int, time.Duration, bool, *AppError) {
		return int(fetches.Add(1)), time.Hour, true, nil
	}
	if value, err := c.get(context.Background(), "persist", nil, fetch); err != nil || value != 1 {
		t.Fatal(value, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openLocalStore(dir, day)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c = newPersistentCache[int](s, "post", make(chan struct{}, 1), 4096)
	meta := &fetchMeta{}
	if value, err := c.get(context.Background(), "persist", meta, fetch); err != nil || value != 1 || meta.fetched {
		t.Fatalf("restart cache=%d,%v,fetched:%v", value, err, meta.fetched)
	}
	if _, err = s.state.Exec("UPDATE models SET expires_at=?", time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	c = newPersistentCache[int](s, "post", make(chan struct{}, 1), 4096)
	if value, err := c.get(context.Background(), "persist", nil, fetch); err != nil || value != 2 {
		t.Fatal("expired persistent value reused", value, err)
	}
	invalid := Post{Shortcode: "Ab_12", Username: "valid_user"}
	encoded, _ := json.Marshal(cachePayload[Post]{Value: invalid})
	if err = s.putModel(context.Background(), "post", "Ab_12", encoded, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	posts := newPersistentCache[Post](s, "post", make(chan struct{}, 1), 4096)
	got, appErr := posts.get(context.Background(), "Ab_12", nil, func(context.Context) (Post, time.Duration, bool, *AppError) {
		return validTestPost("Ab_12", "fresh"), time.Hour, true, nil
	})
	if appErr != nil || got.Caption != "fresh" {
		t.Fatal("invalid disk model not replaced")
	}
}

func TestPersistentCacheStoresOnlySuccessfulEntriesAndPositiveTTL(t *testing.T) {
	s := newTestStore(t)
	c := newPersistentCache[int](s, "post", make(chan struct{}, 1), 4096)
	_, _ = c.get(context.Background(), "error", nil, func(context.Context) (int, time.Duration, bool, *AppError) {
		return 0, time.Hour, true, igErr(404, errorCodeMediaNotFound, "missing")
	})
	_, _ = c.get(context.Background(), "zero", nil, func(context.Context) (int, time.Duration, bool, *AppError) { return 1, 0, true, nil })
	_, _ = c.get(context.Background(), "direct", nil, func(context.Context) (int, time.Duration, bool, *AppError) { return 1, time.Hour, false, nil })
	_, _ = c.get(context.Background(), "ok", nil, func(context.Context) (int, time.Duration, bool, *AppError) { return 1, time.Hour, true, nil })
	var count int
	if err := s.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("successful positive-TTL persistent entries=%d", count)
	}
}

func TestPersistentCacheWriteFailureKeepsLocalSuccess(t *testing.T) {
	s := newTestStore(t)
	_ = s.state.Close()
	c := newPersistentCache[int](s, "post", make(chan struct{}, 1), 4096)
	var calls int
	fetch := func(context.Context) (int, time.Duration, bool, *AppError) { calls++; return 42, time.Hour, true, nil }
	for range 2 {
		if got, err := c.get(context.Background(), "a", nil, fetch); err != nil || got != 42 {
			t.Fatal(got, err)
		}
	}
	if calls != 1 {
		t.Fatal("failed disk write prevented L1 reuse")
	}
}

func TestLocalModelStoreBoundsAndPurgeDoesNotResetBudget(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < persistentModelEntries; i++ {
		if _, err = tx.Exec("INSERT INTO models VALUES ('post', ?, ?, ?, ?)", i, []byte(`{"value":1}`), time.Now().Add(time.Hour).UnixMilli(), i); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.putModel(ctx, "post", "new", []byte(`{"value":2}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != persistentModelEntries {
		t.Fatalf("entry cap=%d", count)
	}
	_, _, err = s.takeBudget(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	remaining := remainingDailyBudget(t, s)
	if err = s.PurgeModels(ctx); err != nil {
		t.Fatal(err)
	}
	if remainingDailyBudget(t, s) != remaining {
		t.Fatal("model purge reset bandwidth ledger")
	}
}

func TestLocalMetricsOriginSeriesAndRetention(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		category  string
		duration  time.Duration
		status    int
		errorType string
	}{
		{"posts", 10 * time.Millisecond, 200, ""}, {"posts", 20 * time.Millisecond, 403, errorCodeGeoBlocked},
		{"stories", 30 * time.Millisecond, 502, errorCodeUpstream}, {"profile", 40 * time.Millisecond, 200, ""},
	} {
		if err := s.RecordMetric(ctx, tc.category, tc.duration, tc.status, tc.errorType); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.state.Exec("INSERT INTO metrics(t,category,duration,outcome) VALUES (?, 'posts', 999, 2)", time.Now().Add(-25*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all := report["all"]
	if len(all.T) != 1 || all.Resolved[0] != 2 || all.Restricted[0] != 1 || all.Failed[0] != 1 || all.P50[0] != 20 || all.P90[0] != 40 || all.P99[0] != 40 {
		t.Fatalf("origin series=%+v", all)
	}
	if report["posts"].Resolved[0] != 1 || report["stories"].Failed[0] != 1 || report["profile"].Resolved[0] != 1 {
		t.Fatal("category aggregation incorrect")
	}
	encoded, err := json.Marshal(emptyStatusReport())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "null") {
		t.Fatal("empty series must contain arrays")
	}
}

func TestLocalMetricsRowAndDiskCaps(t *testing.T) {
	s := newTestStore(t)
	_, err := s.state.Exec(`WITH RECURSIVE numbers(n) AS (
		VALUES (1) UNION ALL SELECT n+1 FROM numbers WHERE n < ?
	) INSERT INTO metrics(t, category, duration, outcome) SELECT ?, 'posts', 1, 0 FROM numbers`, metricEntryCap+10, time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMetric(context.Background(), "posts", time.Millisecond, 200, ""); err != nil {
		t.Fatal(err)
	}
	var count, pages int
	if err := s.state.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > metricEntryCap {
		t.Fatalf("metric rows=%d exceed cap=%d", count, metricEntryCap)
	}
	if err := s.state.QueryRow("PRAGMA max_page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 81920 {
		t.Fatalf("state disk cap=%d pages", pages)
	}
}

func TestLocalBackupAndConservativeRestore(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.putModel(ctx, "post", "a", []byte(`{"value":42}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.takeBudget(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "backup")
	if err := s.Backup(ctx, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := openLocalStore(destination, time.Now().UTC().Format(time.DateOnly))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, _, err = restored.getModel(ctx, "post", "a"); err != nil {
		t.Fatal(err)
	}
	if remainingDailyBudget(t, restored) != proxyDailyByteBudget(time.Now())-proxyByteLeaseSize {
		t.Fatal("backup lost committed WAL budget grant")
	}
	if err = restored.ExhaustBudget(ctx); err != nil {
		t.Fatal(err)
	}
	if grant, _, err := restored.takeBudget(ctx, time.Now()); err != nil || grant != 0 {
		t.Fatalf("restored ledger reopened historical allowance: %d,%v", grant, err)
	}
}
