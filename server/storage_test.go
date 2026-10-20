package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func newTestStore(t *testing.T) *localStore {
	t.Helper()
	s, err := openLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPersistentCacheRestartExpiryAndValidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		s, err := openLocalStore(dir)
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
		synctest.Wait()
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = openLocalStore(dir)
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
	})
}

func TestPersistentCacheStoresOnlySuccessfulEntriesAndPositiveTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestStore(t)
		c := newPersistentCache[int](s, "post", make(chan struct{}, 1), 4096)
		_, _ = c.get(context.Background(), "error", nil, func(context.Context) (int, time.Duration, bool, *AppError) {
			return 0, time.Hour, true, igErr(404, errorCodeMediaNotFound, "missing")
		})
		_, _ = c.get(context.Background(), "zero", nil, func(context.Context) (int, time.Duration, bool, *AppError) { return 1, 0, true, nil })
		_, _ = c.get(context.Background(), "direct", nil, func(context.Context) (int, time.Duration, bool, *AppError) { return 1, time.Hour, false, nil })
		_, _ = c.get(context.Background(), "ok", nil, func(context.Context) (int, time.Duration, bool, *AppError) { return 1, time.Hour, true, nil })
		synctest.Wait()
		var keys string
		if err := s.state.QueryRow("SELECT group_concat(key) FROM models").Scan(&keys); err != nil {
			t.Fatal(err)
		}
		if keys != "ok" {
			t.Fatalf("successful positive-TTL persistent entries=%q", keys)
		}
	})
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

func TestModelStoreReplacesAndSweepsExpired(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestStore(t)
		ctx := context.Background()
		for _, m := range []struct {
			key, value string
			ttl        time.Duration
		}{{"a", `{"value":1}`, time.Hour}, {"b", `{"value":2}`, time.Minute}, {"a", `{"value":"longer"}`, time.Hour}} {
			if err := s.putModel(ctx, "post", m.key, []byte(m.value), time.Now().Add(m.ttl)); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(2 * time.Minute)
		// The next write sweeps the expired "b".
		if err := s.putModel(ctx, "post", "c", []byte(`{"value":3}`), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		var keys string
		if err := s.state.QueryRow("SELECT group_concat(key) FROM (SELECT key FROM models ORDER BY key)").Scan(&keys); err != nil || keys != "a,c" {
			t.Fatalf("keys=%q, %v; want a,c", keys, err)
		}
		if value, _, err := s.getModel(ctx, "post", "a"); err != nil || string(value) != `{"value":"longer"}` {
			t.Fatalf("replacement=%s, %v", value, err)
		}
		var pages int
		if err := s.state.QueryRow("PRAGMA max_page_count").Scan(&pages); err != nil || pages != 786432 {
			t.Fatalf("state disk cap=%d pages, %v", pages, err)
		}
	})
}

func TestExpiredBacklogSweptInBatches(t *testing.T) {
	s := newTestStore(t)
	const expired = 100000
	_, err := s.state.Exec(`WITH RECURSIVE numbers(n) AS (
		VALUES (0) UNION ALL SELECT n+1 FROM numbers WHERE n+1 < ?
	) INSERT INTO models(kind,key,value,expires_at,stored_at)
		SELECT 'post', n, zeroblob(3369), n, n FROM numbers`, expired)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = s.putModel(ctx, "post", "new"+strconv.Itoa(i), []byte(`{"value":1}`), time.Now().Add(time.Hour))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var left int
		if err = s.state.QueryRow("SELECT COUNT(*) FROM models WHERE expires_at <= ?", time.Now().UnixMilli()).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != expired-i*modelSweepBatch {
			t.Fatalf("expired rows after put %d = %d", i, left)
		}
	}
}

func TestOpenDropsOldCapacitySchema(t *testing.T) {
	dir := t.TempDir()
	db, err := openSQLite(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// The model schema of releases up to 61c37f0.
	_, err = db.Exec(`
		CREATE TABLE models (
			kind TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL,
			expires_at INTEGER NOT NULL, stored_at INTEGER NOT NULL,
			PRIMARY KEY (kind, key)
		);
		CREATE INDEX model_expiry ON models(expires_at);
		CREATE INDEX model_age ON models(stored_at);
		CREATE INDEX model_value_size ON models(length(value));
		CREATE TABLE model_usage (id INTEGER PRIMARY KEY CHECK (id = 0), entries INTEGER NOT NULL, bytes INTEGER NOT NULL);
		CREATE TRIGGER model_usage_insert AFTER INSERT ON models BEGIN
			UPDATE model_usage SET entries = entries + 1, bytes = bytes + length(NEW.value);
		END;
		CREATE TRIGGER model_usage_delete AFTER DELETE ON models BEGIN
			UPDATE model_usage SET entries = entries - 1, bytes = bytes - length(OLD.value);
		END;
		INSERT INTO model_usage VALUES (0, 0, 0);
		INSERT INTO models VALUES ('post', 'a', x'01', 9e15, 1);
	`)
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	s, err := openLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var left string
	if err = s.state.QueryRow(`SELECT COALESCE(group_concat(name), '') FROM sqlite_schema WHERE name IN ('model_usage', 'model_usage_insert', 'model_usage_delete', 'model_age', 'model_value_size')`).Scan(&left); err != nil || left != "" {
		t.Fatalf("old capacity schema left: %q, %v", left, err)
	}
	ctx := context.Background()
	if err = s.putModel(ctx, "post", "a", []byte(`{"value":2}`), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if value, _, err := s.getModel(ctx, "post", "a"); err != nil || string(value) != `{"value":2}` {
		t.Fatalf("model=%s, %v", value, err)
	}
}

func TestLocalMetricsOriginSeriesAndRetention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			{"posts", 50 * time.Millisecond, 404, errorCodeNotFound},
		} {
			if err := s.RecordMetric(ctx, tc.category, tc.duration, tc.status, tc.errorType); err != nil {
				t.Fatal(err)
			}
		}
		stale := time.Now().Add(-25 * time.Hour).Unix()
		if _, err := s.state.Exec("INSERT INTO metric_buckets VALUES (?, 'posts', 'old', '[0,0,1]', '[1]')", stale); err != nil {
			t.Fatal(err)
		}
		report, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		near := func(got, want float64) bool { return math.Abs(got/want-1) < .1 }
		all := report["all"]
		if len(all.T) != 1 || all.Resolved[0] != 2 || all.Restricted[0] != 2 || all.Failed[0] != 1 || !near(all.P50[0], 30) || !near(all.P90[0], 50) || !near(all.P99[0], 50) {
			t.Fatalf("origin series=%+v", all)
		}
		if report["posts"].Resolved[0] != 1 || report["posts"].Restricted[0] != 2 || report["stories"].Failed[0] != 1 || report["profile"].Resolved[0] != 1 {
			t.Fatal("category aggregation incorrect")
		}
		var expired int
		if err = s.state.QueryRow("SELECT COUNT(*) FROM metric_buckets WHERE bucket = ?", stale).Scan(&expired); err != nil || expired != 0 {
			t.Fatalf("expired metric rows=%d,%v", expired, err)
		}
		encoded, err := json.Marshal(emptyStatusReport())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "null") {
			t.Fatal("empty series must contain arrays")
		}
	})
}

func TestLocalMetricsSumAcrossInstances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		blue, err := openLocalStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer blue.Close()
		green, err := openLocalStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		for _, s := range []*localStore{blue, green, green} {
			if err = s.RecordMetric(ctx, "posts", time.Millisecond, 200, ""); err != nil {
				t.Fatal(err)
			}
		}
		// Closing flushes the live bucket.
		if err = green.Close(); err != nil {
			t.Fatal(err)
		}
		report, err := blue.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if posts := report["posts"]; len(posts.T) != 1 || posts.Resolved[0] != 3 {
			t.Fatalf("blue/green rows not summed: %+v", posts)
		}
	})
}

func TestCorruptStateDatabaseIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.sqlite"), []byte(strings.Repeat("not sqlite", 1000)), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if aside, _ := filepath.Glob(filepath.Join(dir, "state.sqlite.corrupt-*")); len(aside) != 1 {
		t.Fatalf("corrupt state set aside as %v", aside)
	}
}
