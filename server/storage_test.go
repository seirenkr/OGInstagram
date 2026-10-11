package main

import (
	"context"
	"database/sql"
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

func TestLocalModelStoreBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestStore(t)
		ctx := context.Background()
		_, err := s.state.Exec(`WITH RECURSIVE numbers(n) AS (
			VALUES (0) UNION ALL SELECT n+1 FROM numbers WHERE n+1 < ?
		) INSERT INTO models(kind,key,value,expires_at,stored_at)
			SELECT 'post', n, ?, ?, n FROM numbers`, persistentModelEntries, []byte(`{"value":1}`), time.Now().Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if err = s.putModel(ctx, "post", "new", []byte(`{"value":2}`), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		var count, pages int
		if err = s.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != persistentModelEntries {
			t.Fatalf("entry cap=%d", count)
		}
		if _, _, err = s.getModel(ctx, "post", "0"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("oldest entry not evicted: %v", err)
		}
		for _, key := range []string{"new", strconv.Itoa(persistentModelEntries - 1)} {
			if _, _, err = s.getModel(ctx, "post", key); err != nil {
				t.Fatalf("entry %s evicted: %v", key, err)
			}
		}
		if err = s.state.QueryRow("PRAGMA max_page_count").Scan(&pages); err != nil {
			t.Fatal(err)
		}
		if pages != 81920 {
			t.Fatalf("state disk cap=%d pages", pages)
		}
	})
}

func TestLocalModelStoreReplacementAtCapacity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	expires := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	_, err := s.state.Exec(`WITH RECURSIVE numbers(n) AS (
		VALUES (0) UNION ALL SELECT n+1 FROM numbers WHERE n+1 < ?
	) INSERT INTO models(kind,key,value,expires_at,stored_at)
		SELECT 'post', n, ?, ?, n FROM numbers`, persistentModelEntries, []byte(`{"value":1}`), expires.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	key := strconv.Itoa(persistentModelEntries - 1)
	replacement := []byte(`{"value":22}`)
	if err := s.putModel(ctx, "post", key, replacement, expires); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != persistentModelEntries {
		t.Fatalf("replacement evicted another entry: count=%d", count)
	}
	if _, _, err := s.getModel(ctx, "post", "0"); err != nil {
		t.Fatalf("oldest entry lost during replacement: %v", err)
	}
	value, gotExpiry, err := s.getModel(ctx, "post", key)
	if err != nil || string(value) != string(replacement) || !gotExpiry.Equal(expires) {
		t.Fatalf("replacement value=%s expiry=%s error=%v", value, gotExpiry, err)
	}
	// A rejected insertion must roll back removal of the previous value.
	if _, err := s.state.Exec(`CREATE TRIGGER reject_model BEFORE INSERT ON models
		BEGIN SELECT RAISE(ABORT, 'test rejected write'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.putModel(ctx, "post", key, []byte(`{"value":333}`), expires); err == nil {
		t.Fatal("rejected write succeeded")
	}
	value, gotExpiry, err = s.getModel(ctx, "post", key)
	if err != nil || string(value) != string(replacement) || !gotExpiry.Equal(expires) {
		t.Fatalf("failed replacement lost previous value: %s, %s, %v", value, gotExpiry, err)
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
	if err = s.Healthy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if aside, _ := filepath.Glob(filepath.Join(dir, "state.sqlite.corrupt-*")); len(aside) != 1 {
		t.Fatalf("corrupt state set aside as %v", aside)
	}
}

func TestLocalBackupRestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newTestStore(t)
		ctx := context.Background()
		if err := s.putModel(ctx, "post", "a", []byte(`{"value":42}`), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "backup")
		if err := s.Backup(ctx, destination); err != nil {
			t.Fatal(err)
		}
		restored, err := openLocalStore(destination)
		if err != nil {
			t.Fatal(err)
		}
		defer restored.Close()
		if _, _, err = restored.getModel(ctx, "post", "a"); err != nil {
			t.Fatal(err)
		}
	})
}
