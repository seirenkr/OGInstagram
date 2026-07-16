package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCacheLimitsDistinctFetchesGlobally(t *testing.T) {
	slots := make(chan struct{}, 2)
	c1 := newCache[int](3, slots)
	c2 := newCache[int](3, slots)
	started := make(chan string, 3)
	release := make(chan struct{})
	done := make(chan struct{}, 3)

	launch := func(c *cache[int], key string) {
		go func() {
			c.get(context.Background(), key, nil, func() (int, time.Duration, *AppError) {
				started <- key
				<-release
				return 1, time.Hour, nil
			})
			done <- struct{}{}
		}()
	}
	launch(c1, "a")
	launch(c1, "b")
	launch(c2, "c")

	<-started
	<-started
	select {
	case key := <-started:
		t.Fatalf("third fetch %q started before a slot was released", key)
	case <-time.After(100 * time.Millisecond):
	}

	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("third fetch did not start after a slot was released")
	}
	release <- struct{}{}
	release <- struct{}{}
	for range 3 {
		<-done
	}
}

func TestPersistentCacheMarksOnlySuccessfulFetchesKnown(t *testing.T) {
	var mu sync.Mutex
	known := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodHead:
			if known[key] {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			known[key] = r.Header.Get("X-Cache-Known") == "1"
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1))
	c.get(context.Background(), "ok", nil, func() (int, time.Duration, *AppError) { return 1, time.Hour, nil })
	if !c.known(context.Background(), "ok") {
		t.Fatal("successful fetch was not marked known")
	}
	c.get(context.Background(), "failed", nil, func() (int, time.Duration, *AppError) {
		return 0, time.Hour, &AppError{Status: 404, Reason: reasonMediaNotFound}
	})
	if c.known(context.Background(), "failed") {
		t.Fatal("failed fetch was marked known")
	}
}

func TestUnknownOffloadDoesNotFetch(t *testing.T) {
	var mu sync.Mutex
	methods := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods[r.Method]++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	slots := make(chan struct{}, 1)
	a := &App{
		posts:    newPersistentCache[Post](srv.URL, "post", slots),
		profiles: newPersistentCache[Profile](srv.URL, "profile", slots),
		stories:  newPersistentCache[Story](srv.URL, "story", slots),
	}
	for _, path := range []string{
		"http://example.test/offload/Ab_12/1",
		"http://example.test/offload/@user/1",
		"http://example.test/offload/story/user/123",
	} {
		if got := a.route(httptest.NewRequest(http.MethodGet, path, nil)).status; got != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, got)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if methods[http.MethodHead] != 3 || len(methods) != 1 {
		t.Fatalf("cache requests = %#v, want three HEAD checks only", methods)
	}
}
