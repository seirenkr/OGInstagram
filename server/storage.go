package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	proxyMonthlyBudgetBytes int64 = 100_000_000_000
	persistentModelBytes    int64 = 256 << 20
	persistentModelEntries        = 4096
	metricRetention               = 24 * time.Hour
	metricEntryCap                = 200_000
)

// The expendable model/metric database is separate from the bandwidth ledger.
// A failed cache write may be ignored; a failed budget commit must deny traffic.
type localStore struct {
	state        *sql.DB
	budget       *sql.DB
	mu           sync.Mutex
	metricWrites uint64
}

func openLocalStore(dataDir, budgetStartDate string) (*localStore, error) {
	if _, err := time.Parse(time.DateOnly, budgetStartDate); err != nil {
		return nil, fmt.Errorf("PROXY_BUDGET_START_DATE must be YYYY-MM-DD: %w", err)
	}
	if dataDir == "" {
		return nil, errors.New("DATA_DIR must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	_, stateErr := os.Stat(filepath.Join(dataDir, "state.sqlite"))
	s := &localStore{}
	var err error
	if s.state, err = openSQLite(filepath.Join(dataDir, "state.sqlite"), "NORMAL", 81920); err != nil {
		return nil, err
	}
	if s.budget, err = openSQLite(filepath.Join(dataDir, "budget.sqlite"), "FULL", 1024); err != nil {
		_ = s.state.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lostLedger := false
	if stateErr == nil {
		// An empty SQLite file is structurally valid. Inspect the ledger before
		// CREATE/INSERT can hide a missing file, truncation or lost metadata.
		var tables int
		if err = s.budget.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('budget_config', 'budget_days')").Scan(&tables); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("inspect existing budget ledger: %w", err)
		}
		lostLedger = tables != 2
		if !lostLedger {
			var lastDay string
			err = s.budget.QueryRowContext(ctx, "SELECT last_day FROM budget_config WHERE id = 1").Scan(&lastDay)
			if errors.Is(err, sql.ErrNoRows) {
				lostLedger = true
			} else if err != nil {
				_ = s.Close()
				return nil, fmt.Errorf("inspect existing budget metadata: %w", err)
			} else if lastDay != "" {
				var recorded bool
				if err = s.budget.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM budget_days WHERE day = ?)", lastDay).Scan(&recorded); err != nil {
					_ = s.Close()
					return nil, fmt.Errorf("inspect existing budget usage: %w", err)
				}
				lostLedger = !recorded
			}
		}
	}
	_, err = s.state.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS models (
			kind TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL,
			expires_at INTEGER NOT NULL, stored_at INTEGER NOT NULL,
			PRIMARY KEY (kind, key)
		);
		CREATE INDEX IF NOT EXISTS model_expiry ON models(expires_at);
		CREATE INDEX IF NOT EXISTS model_age ON models(stored_at);
		CREATE TABLE IF NOT EXISTS metrics (
			id INTEGER PRIMARY KEY, t INTEGER NOT NULL, category TEXT NOT NULL,
			duration REAL NOT NULL, outcome INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS metric_time ON metrics(t);
	`)
	if err == nil {
		_, err = s.budget.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS budget_config (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				start_date TEXT NOT NULL, last_day TEXT NOT NULL
			);
			CREATE TABLE IF NOT EXISTS budget_days (
				day TEXT PRIMARY KEY, used INTEGER NOT NULL CHECK (used >= 0)
			);
			INSERT INTO budget_config(id, start_date, last_day) VALUES (1, ?, '')
			ON CONFLICT(id) DO UPDATE SET start_date = MAX(start_date, excluded.start_date);
		`, budgetStartDate)
	}
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("initialize local storage: %w", err)
	}
	// A surviving state database proves this is not a fresh installation. A
	// lost ledger cannot safely regain an allowance spent earlier today.
	if lostLedger {
		if err := s.ExhaustBudget(ctx); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("close lost ledger allowance: %w", err)
		}
	}
	return s, nil
}

func openSQLite(path, synchronous string, maxPages int) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Create with private permissions before SQLite opens its WAL and shared
	// memory files, which inherit the database's permissions.
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = errors.Join(file.Chmod(0600), file.Close()); err != nil {
		return nil, err
	}
	params := url.Values{}
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(" + synchronous + ")", "busy_timeout(1000)", "cache_size(-2048)", "journal_size_limit(8388608)", fmt.Sprintf("max_page_count(%d)", maxPages)} {
		params.Add("_pragma", pragma)
	}
	params.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: params.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection bounds SQLite page cache and serializes local writers.
	// BEGIN IMMEDIATE also serializes independent processes using the same volume.
	db.SetMaxOpenConns(1)
	var integrity string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&integrity); err == nil && integrity != "ok" {
		err = fmt.Errorf("SQLite integrity check: %s", integrity)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err = os.Chmod(abs, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (s *localStore) Close() error { return errors.Join(s.state.Close(), s.budget.Close()) }

func (s *localStore) Healthy(ctx context.Context) error {
	if s == nil {
		return errors.New("local storage unavailable")
	}
	var count int
	if err := s.state.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('models', 'metrics')").Scan(&count); err != nil {
		return err
	}
	if count != 2 {
		return errors.New("model and metric schema unavailable")
	}
	var start string
	return s.budget.QueryRowContext(ctx, "SELECT start_date FROM budget_config WHERE id = 1").Scan(&start)
}

func (s *localStore) getModel(ctx context.Context, kind, key string) ([]byte, time.Time, error) {
	var value []byte
	var expires int64
	err := s.state.QueryRowContext(ctx, "SELECT value, expires_at FROM models WHERE kind = ? AND key = ? AND expires_at > ?", kind, key, time.Now().UnixMilli()).Scan(&value, &expires)
	return value, time.UnixMilli(expires), err
}

func (s *localStore) putModel(ctx context.Context, kind, key string, value []byte, expires time.Time) error {
	if len(value) > maxCacheValueBytes || !expires.After(time.Now()) {
		return nil
	}
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM models WHERE expires_at <= ?", time.Now().UnixMilli()); err != nil {
		return err
	}
	// Evict before insertion so the on-disk page ceiling cannot prevent cleanup.
	var count int
	var used int64
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(length(value)), 0) FROM models").Scan(&count, &used); err != nil {
		return err
	}
	for count >= persistentModelEntries || used+int64(len(value)) > persistentModelBytes {
		var oldestKind, oldestKey string
		var size int64
		if err = tx.QueryRowContext(ctx, "SELECT kind, key, length(value) FROM models ORDER BY stored_at LIMIT 1").Scan(&oldestKind, &oldestKey, &size); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM models WHERE kind = ? AND key = ?", oldestKind, oldestKey); err != nil {
			return err
		}
		count--
		used -= size
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO models(kind, key, value, expires_at, stored_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kind, key) DO UPDATE SET value=excluded.value, expires_at=excluded.expires_at, stored_at=excluded.stored_at`, kind, key, value, expires.UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *localStore) PurgeModels(ctx context.Context) error {
	_, err := s.state.ExecContext(ctx, "DELETE FROM models")
	return err
}

func proxyDailyByteBudget(now time.Time) int64 {
	now = now.UTC()
	days := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return proxyMonthlyBudgetBytes / int64(days)
}

// A grant is charged durably before bytes may leave the process. Unused local
// leases are intentionally lost on restart; they never reset the daily ledger.
func (s *localStore) takeBudget(ctx context.Context, now time.Time) (int64, time.Time, error) {
	if s == nil {
		return 0, time.Time{}, errors.New("budget storage unavailable")
	}
	now = now.UTC()
	day := now.Format(time.DateOnly)
	expires := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	tx, err := s.budget.BeginTx(ctx, nil)
	if err != nil {
		return 0, expires, err
	}
	defer tx.Rollback()
	var start, lastDay string
	if err = tx.QueryRowContext(ctx, "SELECT start_date, last_day FROM budget_config WHERE id = 1").Scan(&start, &lastDay); err != nil {
		return 0, expires, err
	}
	if day < start || day < lastDay {
		return 0, expires, nil
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO budget_days(day, used) VALUES (?, 0) ON CONFLICT(day) DO NOTHING", day); err != nil {
		return 0, expires, err
	}
	var used int64
	if err = tx.QueryRowContext(ctx, "SELECT used FROM budget_days WHERE day = ?", day).Scan(&used); err != nil {
		return 0, expires, err
	}
	grant := min(proxyByteLeaseSize, max(int64(0), proxyDailyByteBudget(now)-used))
	if _, err = tx.ExecContext(ctx, "UPDATE budget_days SET used = used + ? WHERE day = ?", grant, day); err != nil {
		return 0, expires, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE budget_config SET last_day = ? WHERE id = 1", day); err != nil {
		return 0, expires, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM budget_days WHERE day < ?", now.AddDate(0, 0, -64).Format(time.DateOnly)); err != nil {
		return 0, expires, err
	}
	if err = tx.Commit(); err != nil {
		return 0, expires, err
	}
	return grant, expires, nil
}

// Use after restoring a historical backup. Its ledger cannot account for bytes
// spent after that snapshot, so conservatively close today's remaining budget.
func (s *localStore) ExhaustBudget(ctx context.Context) error {
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	tx, err := s.budget.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO budget_days(day, used) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET used = MAX(used, excluded.used)`, day, proxyDailyByteBudget(now)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE budget_config SET last_day = MAX(last_day, ?) WHERE id = 1", day); err != nil {
		return err
	}
	return tx.Commit()
}

type StatusSeries struct {
	T          []int64   `json:"t"`
	P50        []float64 `json:"p50"`
	P90        []float64 `json:"p90"`
	P99        []float64 `json:"p99"`
	Resolved   []int64   `json:"resolved"`
	Restricted []int64   `json:"restricted"`
	Failed     []int64   `json:"failed"`
}
type StatusReport map[string]*StatusSeries

func emptyStatusReport() StatusReport {
	r := StatusReport{}
	for _, category := range []string{"all", "posts", "stories", "profile"} {
		r[category] = &StatusSeries{T: []int64{}, P50: []float64{}, P90: []float64{}, P99: []float64{}, Resolved: []int64{}, Restricted: []int64{}, Failed: []int64{}}
	}
	return r
}

func (s *localStore) RecordMetric(ctx context.Context, category string, duration time.Duration, status int, errorType string) error {
	outcome := 0
	if status >= 400 {
		outcome = 2
		if errorType == errorCodeGeoBlocked {
			outcome = 1
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.metricWrites%128 == 0 {
		// Bounded so a backlog cannot outgrow the caller's short deadline.
		if _, err = tx.ExecContext(ctx, "DELETE FROM metrics WHERE id IN (SELECT id FROM metrics WHERE t < ? LIMIT 1024)", time.Now().Add(-metricRetention).Unix()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM metrics WHERE id <= (SELECT id FROM metrics ORDER BY id DESC LIMIT 1 OFFSET ?)", metricEntryCap-128); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO metrics(t, category, duration, outcome) VALUES (?, ?, ?, ?)", time.Now().Unix(), category, max(float64(0), float64(duration)/float64(time.Millisecond)), outcome)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.metricWrites++
	return nil
}

type metricBucket struct {
	durations []float64
	outcomes  [3]int64
}

func (s *localStore) Status(ctx context.Context) (StatusReport, error) {
	report := emptyStatusReport()
	rows, err := s.state.QueryContext(ctx, "SELECT t/600*600, category, duration, outcome FROM metrics WHERE t >= ? ORDER BY t, id", time.Now().Add(-metricRetention).Unix())
	if err != nil {
		return report, err
	}
	defer rows.Close()
	buckets := map[string]map[int64]*metricBucket{}
	for rows.Next() {
		var t int64
		var category string
		var duration float64
		var outcome int
		if err = rows.Scan(&t, &category, &duration, &outcome); err != nil {
			return report, err
		}
		if report[category] == nil || outcome < 0 || outcome > 2 {
			continue
		}
		for _, key := range []string{category, "all"} {
			if buckets[key] == nil {
				buckets[key] = map[int64]*metricBucket{}
			}
			if buckets[key][t] == nil {
				buckets[key][t] = &metricBucket{}
			}
			bucket := buckets[key][t]
			bucket.durations = append(bucket.durations, duration)
			bucket.outcomes[outcome]++
		}
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	for category, grouped := range buckets {
		for _, t := range slices.Sorted(maps.Keys(grouped)) {
			bucket := grouped[t]
			slices.Sort(bucket.durations)
			quantile := func(q float64) float64 {
				index := max(0, int(math.Ceil(q*float64(len(bucket.durations))))-1)
				return math.Round(bucket.durations[index]*100) / 100
			}
			series := report[category]
			series.T = append(series.T, t)
			series.P50 = append(series.P50, quantile(.5))
			series.P90 = append(series.P90, quantile(.9))
			series.P99 = append(series.P99, quantile(.99))
			series.Resolved = append(series.Resolved, bucket.outcomes[0])
			series.Restricted = append(series.Restricted, bucket.outcomes[1])
			series.Failed = append(series.Failed, bucket.outcomes[2])
		}
	}
	return report, nil
}

// VACUUM INTO captures committed data including WAL without copying live files.
// Each database has its own consistent snapshot; cache and budget have no shared
// transaction. The destination must be new. ExhaustBudget is required on restore.
func (s *localStore) Backup(ctx context.Context, destinationDir string) error {
	if err := os.Mkdir(destinationDir, 0700); err != nil {
		return err
	}
	for name, db := range map[string]*sql.DB{"state.sqlite": s.state, "budget.sqlite": s.budget} {
		path, err := filepath.Abs(filepath.Join(destinationDir, name))
		if err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
			return err
		}
		if err = os.Chmod(path, 0600); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = errors.Join(file.Sync(), file.Close())
		if err != nil {
			return err
		}
	}
	return nil
}
