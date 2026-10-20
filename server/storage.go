package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	modelSweepBatch     = 512
	metricRetention     = 24 * time.Hour
	metricBucketSeconds = 600
	metricLatencyBins   = 64
)

var errCorruptSQLite = errors.New("corrupt SQLite database")

type localStore struct {
	state *sql.DB
	// Metrics accumulate per process and are upserted under this random id, so
	// blue and green slots sharing the volume never overwrite each other's rows.
	instance string
	mu       sync.Mutex
	bucket   int64
	pending  map[metricKey]*metricBucket
}

type metricKey struct {
	bucket   int64
	category string
}

type metricBucket struct {
	outcomes [3]int64
	latency  [metricLatencyBins]int64
}

func openLocalStore(dataDir string) (*localStore, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	statePath := filepath.Join(dataDir, "state.sqlite")
	s := &localStore{instance: rand.Text(), pending: map[metricKey]*metricBucket{}}
	var err error
	s.state, err = openSQLite(statePath)
	if errors.Is(err, errCorruptSQLite) {
		// Models and metrics are expendable: keep the damaged file for inspection
		// and start empty instead of refusing to serve.
		aside := fmt.Sprintf("%s.corrupt-%d", statePath, time.Now().Unix())
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(statePath+suffix, aside+suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		slog.Warn("replaced corrupt state database", "error", err, "moved_to", aside)
		s.state, err = openSQLite(statePath)
	}
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = s.initSchema(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("initialize local storage: %w", err)
	}
	return s, nil
}

// Model TTLs are capped at 24 h, so expiry bounds the table; the DROPs clear
// the capacity bookkeeping older releases created.
func (s *localStore) initSchema(ctx context.Context) error {
	_, err := s.state.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS models (
			kind TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL,
			expires_at INTEGER NOT NULL, stored_at INTEGER NOT NULL,
			PRIMARY KEY (kind, key)
		);
		CREATE INDEX IF NOT EXISTS model_expiry ON models(expires_at);
		DROP TRIGGER IF EXISTS model_usage_insert;
		DROP TRIGGER IF EXISTS model_usage_delete;
		DROP TABLE IF EXISTS model_usage;
		DROP INDEX IF EXISTS model_age;
		DROP INDEX IF EXISTS model_value_size;
		CREATE TABLE IF NOT EXISTS metric_buckets (
			bucket INTEGER NOT NULL, category TEXT NOT NULL, instance TEXT NOT NULL,
			outcomes TEXT NOT NULL, latency TEXT NOT NULL,
			PRIMARY KEY (bucket, category, instance)
		);
	`)
	return err
}

func openSQLite(path string) (*sql.DB, error) {
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
	// max_page_count caps the file at 3 GiB (4 KiB pages).
	for _, pragma := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(1000)", "cache_size(-2048)", "journal_size_limit(8388608)", "max_page_count(786432)"} {
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
	err = db.QueryRow("PRAGMA quick_check").Scan(&integrity)
	var sqliteErr *sqlite.Error
	if err == nil && integrity != "ok" {
		err = fmt.Errorf("%w: %s", errCorruptSQLite, integrity)
	} else if errors.As(err, &sqliteErr) && (sqliteErr.Code()&0xff == sqlite3.SQLITE_CORRUPT || sqliteErr.Code()&0xff == sqlite3.SQLITE_NOTADB) {
		err = fmt.Errorf("%w: %w", errCorruptSQLite, err)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func (s *localStore) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.mu.Lock()
	err := s.flushMetrics(ctx, time.Now())
	s.mu.Unlock()
	return errors.Join(err, s.state.Close())
}

func (s *localStore) getModel(ctx context.Context, kind, key string) ([]byte, time.Time, error) {
	var value []byte
	var expires int64
	err := s.state.QueryRowContext(ctx, "SELECT value, expires_at FROM models WHERE kind = ? AND key = ? AND expires_at > ?", kind, key, time.Now().UnixMilli()).Scan(&value, &expires)
	return value, time.UnixMilli(expires), err
}

func (s *localStore) putModel(ctx context.Context, kind, key string, value []byte, expires time.Time) error {
	now := time.Now()
	if !expires.After(now) {
		return nil
	}
	// A bounded sweep keeps a backlog left by hours without writes from
	// overrunning the put deadline.
	if _, err := s.state.ExecContext(ctx, "DELETE FROM models WHERE rowid IN (SELECT rowid FROM models WHERE expires_at <= ? ORDER BY expires_at LIMIT ?)", now.UnixMilli(), modelSweepBatch); err != nil {
		return err
	}
	_, err := s.state.ExecContext(ctx, "INSERT OR REPLACE INTO models(kind, key, value, expires_at, stored_at) VALUES (?, ?, ?, ?, ?)", kind, key, value, expires.UnixMilli(), now.UnixMilli())
	return err
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
		if !isTransient(errorType) {
			outcome = 1
		}
	}
	// Bin i holds latencies in (2^((i-1)/4), 2^(i/4)] ms.
	bin := min(metricLatencyBins-1, int(math.Ceil(4*math.Log2(max(1, float64(duration)/float64(time.Millisecond))))))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if now.Unix()/metricBucketSeconds*metricBucketSeconds != s.bucket {
		err = s.flushMetrics(ctx, now)
	}
	key := metricKey{s.bucket, category}
	if s.pending[key] == nil {
		s.pending[key] = &metricBucket{}
	}
	s.pending[key].outcomes[outcome]++
	s.pending[key].latency[bin]++
	return err
}

// flushMetrics upserts this process's running totals and forgets closed buckets
// once they are stored. The caller holds s.mu.
func (s *localStore) flushMetrics(ctx context.Context, now time.Time) error {
	s.bucket = now.Unix() / metricBucketSeconds * metricBucketSeconds
	if len(s.pending) == 0 {
		return nil
	}
	tx, err := s.state.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, m := range s.pending {
		outcomes, _ := json.Marshal(m.outcomes)
		latency, _ := json.Marshal(m.latency)
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO metric_buckets VALUES (?, ?, ?, ?, ?)", key.bucket, key.category, s.instance, string(outcomes), string(latency)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM metric_buckets WHERE bucket < ?", now.Add(-metricRetention).Unix()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	maps.DeleteFunc(s.pending, func(key metricKey, _ *metricBucket) bool { return key.bucket != s.bucket })
	return nil
}

func (s *localStore) Status(ctx context.Context) (StatusReport, error) {
	report := emptyStatusReport()
	now := time.Now()
	s.mu.Lock()
	err := s.flushMetrics(ctx, now)
	s.mu.Unlock()
	if err != nil {
		return report, err
	}
	rows, err := s.state.QueryContext(ctx, "SELECT bucket, category, outcomes, latency FROM metric_buckets WHERE bucket >= ?", now.Add(-metricRetention).Unix())
	if err != nil {
		return report, err
	}
	defer rows.Close()
	buckets := map[metricKey]*metricBucket{}
	for rows.Next() {
		var t int64
		var category, outcomes, latency string
		if err = rows.Scan(&t, &category, &outcomes, &latency); err != nil {
			return report, err
		}
		var row metricBucket
		if report[category] == nil || json.Unmarshal([]byte(outcomes), &row.outcomes) != nil || json.Unmarshal([]byte(latency), &row.latency) != nil {
			continue
		}
		for _, key := range []metricKey{{t, category}, {t, "all"}} {
			if buckets[key] == nil {
				buckets[key] = &metricBucket{}
			}
			bucket := buckets[key]
			for i, n := range row.outcomes {
				bucket.outcomes[i] += n
			}
			for i, n := range row.latency {
				bucket.latency[i] += n
			}
		}
	}
	if err = rows.Err(); err != nil {
		return report, err
	}
	for _, key := range slices.SortedFunc(maps.Keys(buckets), func(a, b metricKey) int { return cmp.Compare(a.bucket, b.bucket) }) {
		bucket := buckets[key]
		var total int64
		for _, n := range bucket.latency {
			total += n
		}
		quantile := func(q float64) float64 {
			rank := int64(math.Ceil(q * float64(total)))
			for i, n := range bucket.latency {
				if rank -= n; rank <= 0 {
					// A bin's geometric centre is within 10% of every latency in it.
					return math.Round(math.Exp2((float64(i)-.5)/4)*100) / 100
				}
			}
			return 0
		}
		series := report[key.category]
		series.T = append(series.T, key.bucket)
		series.P50 = append(series.P50, quantile(.5))
		series.P90 = append(series.P90, quantile(.9))
		series.P99 = append(series.P99, quantile(.99))
		series.Resolved = append(series.Resolved, bucket.outcomes[0])
		series.Restricted = append(series.Restricted, bucket.outcomes[1])
		series.Failed = append(series.Failed, bucket.outcomes[2])
	}
	return report, nil
}
