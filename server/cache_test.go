package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func stubDefaultTransport(t *testing.T, transport http.RoundTripper) {
	previous := http.DefaultClient.Transport
	http.DefaultClient.Transport = transport
	t.Cleanup(func() { http.DefaultClient.Transport = previous })
}

const simpleEmbedImagePage = `<script>["PolarisEmbedSimple","init",[],[{"isRichEmbed":false,"contextJSON":null}]]</script>` +
	`<div class="Embed" data-media-type="GraphImage" data-media-id="3928250036051888465" data-owner-id="25025320" data-permalink="https://www.instagram.com/p/DaD8phTyclR/?utm_source=ig_embed">` +
	`<a class="Avatar InsideRing" href="https://www.instagram.com/instagram/?utm_source=ig_embed"><img src="https://cdn/avatar.jpg?oe=1" alt="instagram" /></a>` +
	`<span class="UsernameText">instagram</span>` +
	`<div class="Content EmbedFrame" style="padding-bottom: 133.33%;">` +
	`<img class="EmbeddedMediaImage" alt="x" src="https://cdn/small.jpg?oe=1" srcset="https://cdn/big.jpg?oe=1 3072w,https://cdn/small.jpg?oe=1 640w" /></div>` +
	`<div class="SocialProof"><a href="/x">4,809 likes</a></div>` +
	`<div class="Caption"><a class="CaptionUsername" href="/x">instagram</a><br /><br />Hello &amp; <a href="/explore/tags/x">#world</a><br />line2` +
	`<div class="CaptionComments"><a href="/x">View all 19 comments</a></div></div>`

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCacheLimitsDistinctFetchesGlobally(t *testing.T) {
	slots := make(chan struct{}, 2)
	c1 := newCache[int](3, 1<<20, slots)
	c2 := newCache[int](3, 1<<20, slots)
	started := make(chan string, 3)
	release := make(chan struct{})
	done := make(chan struct{}, 3)

	launch := func(c *cache[int], key string) {
		go func() {
			c.get(context.Background(), key, nil, func(context.Context) (int, time.Duration, *AppError) {
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

func TestPersistentCacheStoresOnlySuccessfulEntries(t *testing.T) {
	var mu sync.Mutex
	methods := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods[r.Method]++
		mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1), 0)
	c.get(context.Background(), "failed", nil, func(context.Context) (int, time.Duration, *AppError) {
		return 0, time.Hour, igErr(http.StatusNotFound, errorCodeMediaNotFound, "not found")
	})
	c.get(context.Background(), "ok", nil, func(context.Context) (int, time.Duration, *AppError) {
		return 1, time.Hour, nil
	})
	waitUntil(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return methods[http.MethodGet] == 2 && methods[http.MethodPost] == 0 &&
			methods[http.MethodPut] == 1
	})
}

func TestCacheModelBoundaryNormalizesCDNHosts(t *testing.T) {
	post, valid := cloneAndValidateCacheValue("Ab_12", Post{
		Shortcode:  "Ab_12",
		Username:   "tester",
		ProfilePic: "https://profile-a.fbcdn.net/avatar.jpg",
		Attachments: []Attachment{{
			Kind:      "video",
			URL:       "https://video-a.fbcdn.net/video.mp4",
			Thumbnail: "https://image-a.cdninstagram.com/thumb.jpg",
		}},
	})
	if !valid {
		t.Fatal("valid post model was rejected")
	}
	for _, got := range []string{post.ProfilePic, post.Attachments[0].URL, post.Attachments[0].Thumbnail} {
		if got == "" || !strings.HasPrefix(got, "https://scontent.cdninstagram.com/") {
			t.Fatalf("cache boundary did not normalize CDN URL: %q", got)
		}
	}
}

func TestPersistentCacheLocalByteBudgetSkipsRemoteAndEvicts(t *testing.T) {
	var mu sync.Mutex
	gets := map[string]int{}
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		gets[r.URL.Query().Get("key")]++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	sampleSize := cacheEntryMemorySize("a", &cacheEntry[int]{value: 1, expiresAt: time.Now().Add(time.Hour)})
	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1), sampleSize*3)
	fetch := func(v int) func(context.Context) (int, time.Duration, *AppError) {
		return func(context.Context) (int, time.Duration, *AppError) { return v, time.Hour, nil }
	}

	c.get(context.Background(), "a", nil, fetch(1))
	c.get(context.Background(), "b", nil, fetch(2))
	c.get(context.Background(), "c", nil, fetch(3))

	c.get(context.Background(), "d", nil, fetch(4))

	dummy := fetch(-1)
	for _, key := range []string{"b", "c", "d"} {
		if v, err := c.get(context.Background(), key, nil, dummy); err != nil || v <= 0 {
			t.Fatalf("get(%q) = (%d, %v), want cached positive value with no error", key, v, err)
		}
	}

	c.get(context.Background(), "a", nil, fetch(1))

	mu.Lock()
	want := map[string]int{"post:a": 2, "post:b": 1, "post:c": 1, "post:d": 1}
	for key, count := range want {
		if gets[key] != count {
			t.Errorf("remote GET count for %q = %d, want %d (gets=%#v)", key, gets[key], count, gets)
		}
	}
	mu.Unlock()
	waitUntil(t, time.Second, func() bool { return puts.Load() == 5 })
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStatusRequestStoresEntryUsedByOffload(t *testing.T) {
	var entries sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		switch r.Method {
		case http.MethodGet:
			stored, ok := entries.Load(key)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(stored.([]byte))
		case http.MethodPut:
			if r.Header.Get("X-Cache-Known") != "" {
				t.Errorf("successful model PUT retained legacy X-Cache-Known")
			}
			body, _ := io.ReadAll(r.Body)
			entries.Store(key, body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	directCalls := 0
	slots := make(chan struct{}, 1)
	stubDefaultTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(simpleEmbedImagePage))}, nil
	}))
	a := &App{
		cfg:           Config{BaseURL: "https://example.test"},
		offloadSigner: mustOffloadSigner(testOffloadSigningKeys),
		posts:         newPersistentCache[Post](srv.URL, "post", slots, 0),
	}

	code := statusSnowcode("p", "DaD8phTyclR", 0, false, false)
	status := a.route(httptest.NewRequest(http.MethodGet, "https://example.test/users/instagram/statuses/"+code, nil))
	if status.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status.status)
	}

	waitUntil(t, time.Second, func() bool {
		_, stored := entries.Load("post:DaD8phTyclR")
		return stored
	})

	offload := httptest.NewRequest(http.MethodGet, "https://example.test/offload/DaD8phTyclR/1", nil)
	offload.Header.Set(headerAllowOriginFetch, "1")
	media := a.route(offload)
	if media.status != http.StatusFound || directCalls != 1 {
		t.Fatalf("offload = %d, Instagram fetches = %d; want 302 and one fetch", media.status, directCalls)
	}
}

func TestUnknownOffloadDoesNotFetchOrigin(t *testing.T) {
	var remoteGets atomic.Int32
	var originFetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			remoteGets.Add(1)
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	stubDefaultTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		originFetches.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(simpleEmbedImagePage)),
		}, nil
	}))
	a := &App{
		posts: newPersistentCache[Post](
			srv.URL, "post", make(chan struct{}, 1), localPostCacheBytes,
		),
	}
	result := a.route(httptest.NewRequest(
		http.MethodGet, "https://example.test/offload/Unknown_1/1", nil,
	))
	if result.status != http.StatusNotFound || result.headers["Cache-Control"] != "no-store" {
		t.Fatalf("unknown offload = %#v, want no-store 404", result)
	}
	if got := originFetches.Load(); got != 0 {
		t.Fatalf("origin fetches = %d, want 0", got)
	}
	if got := remoteGets.Load(); got != 0 {
		t.Fatalf("model GETs = %d, want 0 before known-media authorization", got)
	}
}

func TestEdgeAuthorizedDirectOffloadMayFetchOrigin(t *testing.T) {
	var originFetches atomic.Int32
	postCache := newCache[Post](4, 1<<20, make(chan struct{}, 1))
	stubDefaultTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		originFetches.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(simpleEmbedImagePage)),
		}, nil
	}))
	a := &App{
		posts: postCache,
	}
	req := httptest.NewRequest(http.MethodGet, "https://example.test/offload/DaD8phTyclR/1", nil)
	req.Header.Set(headerAllowOriginFetch, "1")
	result := a.route(req)
	if result.status != http.StatusFound {
		t.Fatalf("authorized offload status = %d, want 302", result.status)
	}
	if got := originFetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want 1", got)
	}
}

func TestOffloadRejectsNonCanonicalSuffixBeforeLookup(t *testing.T) {
	a := &App{}
	for _, path := range []string{
		"/offload/Ab_12/anything",
		"/offload/Ab_12/0",
		"/offload/Ab_12/01",
		"/offload/Ab_12/1.mp4.mp4",
		"/offload/Ab_12/51",
		"/offload/@User.Name/01",
		"/offload/@User.Name/7",
		"/offload/@bad!/avatar",
	} {
		result := a.route(httptest.NewRequest(http.MethodGet, "https://example.test"+path, nil))
		if result.status != http.StatusNotFound || result.headers["Cache-Control"] != "no-store" {
			t.Errorf("%s = %#v, want no-store 404", path, result)
		}
	}
}

func TestPersistentCacheRemoteHitWarmsLocalUntilExpiry(t *testing.T) {
	body, err := json.Marshal(cachePayload[int]{Value: 42})
	if err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		gets.Add(1)
		w.Header().Set("X-Cache-Expires", unixMillis(time.Now().Add(time.Hour)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1), 4096)
	var fetches atomic.Int32
	fetch := func(context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		return -1, time.Hour, nil
	}
	for i := 0; i < 2; i++ {
		value, appErr := c.get(context.Background(), "remote", nil, fetch)
		if appErr != nil || value != 42 {
			t.Fatalf("get #%d = (%d, %#v), want (42, nil)", i+1, value, appErr)
		}
	}
	if got := gets.Load(); got != 1 {
		t.Fatalf("remote GETs = %d, want 1", got)
	}
	if got := fetches.Load(); got != 0 {
		t.Fatalf("origin fetches = %d, want 0", got)
	}
}

func TestPersistentCacheRejectsExpiredRemoteValue(t *testing.T) {
	body, err := json.Marshal(cachePayload[int]{Value: 42})
	if err != nil {
		t.Fatal(err)
	}
	var gets atomic.Int32
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			w.Header().Set("X-Cache-Expires", unixMillis(time.Now().Add(-time.Second)))
			_, _ = w.Write(body)
		case http.MethodPut:
			puts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1), 4096)
	var fetches atomic.Int32
	fetch := func(context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		return 7, time.Hour, nil
	}
	for i := 0; i < 2; i++ {
		value, appErr := c.get(context.Background(), "expired", nil, fetch)
		if appErr != nil || value != 7 {
			t.Fatalf("get #%d = (%d, %#v), want fresh (7, nil)", i+1, value, appErr)
		}
	}
	if got := gets.Load(); got != 1 {
		t.Fatalf("remote GETs = %d, want 1 followed by an L1 hit", got)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want 1", got)
	}
	waitUntil(t, time.Second, func() bool { return puts.Load() == 1 })
}

func TestCacheLeaderCancellationDoesNotPoisonSharedWork(t *testing.T) {
	c := newCache[int](4, 1<<20, make(chan struct{}, 1))
	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(fetchCtx context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		close(started)
		select {
		case <-release:
			return 7, time.Hour, nil
		case <-fetchCtx.Done():
			return 0, 0, contextAppError(fetchCtx)
		}
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan *AppError, 1)
	go func() {
		_, appErr := c.get(leaderCtx, "shared", nil, fetch)
		leaderDone <- appErr
	}()
	<-started
	waiterDone := make(chan struct {
		value int
		err   *AppError
	}, 1)
	go func() {
		value, appErr := c.get(context.Background(), "shared", nil, fetch)
		waiterDone <- struct {
			value int
			err   *AppError
		}{value, appErr}
	}()
	cancelLeader()
	select {
	case appErr := <-leaderDone:
		if appErr == nil || appErr.Status != 499 || !appErr.Ephemeral {
			t.Fatalf("cancelled leader error = %#v, want ephemeral 499", appErr)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled leader did not return promptly")
	}
	close(release)

	select {
	case result := <-waiterDone:
		if result.err != nil || result.value != 7 {
			t.Fatalf("waiter result = (%d, %#v), want (7, nil)", result.value, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not receive shared result")
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want one shared fetch", got)
	}
}

func TestCacheLeaderDeadlineDoesNotBoundSharedWork(t *testing.T) {
	type contextKey struct{}
	const contextValue = "request-id"

	c := newCache[int](4, 1<<20, make(chan struct{}, 1))
	c.workTimeout = time.Second
	started := make(chan *AppError, 1)
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(fetchCtx context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		if got := fetchCtx.Value(contextKey{}); got != contextValue {
			appErr := ephemeralErr(500, errorCodeUpstream, "request context value was not preserved")
			started <- appErr
			return 0, 0, appErr
		}
		deadline, ok := fetchCtx.Deadline()
		if !ok || time.Until(deadline) < 500*time.Millisecond {
			appErr := ephemeralErr(500, errorCodeUpstream, "shared work inherited the leader deadline")
			started <- appErr
			return 0, 0, appErr
		}
		started <- nil
		select {
		case <-release:
			return 11, time.Hour, nil
		case <-fetchCtx.Done():
			return 0, 0, contextAppError(fetchCtx)
		}
	}

	parent := context.WithValue(context.Background(), contextKey{}, contextValue)
	leaderCtx, cancelLeader := context.WithTimeout(parent, 100*time.Millisecond)
	defer cancelLeader()
	leaderDone := make(chan *AppError, 1)
	go func() {
		_, appErr := c.get(leaderCtx, "deadline", nil, fetch)
		leaderDone <- appErr
	}()
	if appErr := <-started; appErr != nil {
		t.Fatal(appErr.PublicMessage)
	}
	waiterDone := make(chan struct {
		value int
		err   *AppError
	}, 1)
	go func() {
		value, appErr := c.get(context.Background(), "deadline", nil, fetch)
		waiterDone <- struct {
			value int
			err   *AppError
		}{value, appErr}
	}()
	select {
	case appErr := <-leaderDone:
		if appErr == nil || appErr.Status != http.StatusGatewayTimeout {
			t.Fatalf("deadline leader error = %#v, want 504", appErr)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline leader did not return promptly")
	}
	close(release)
	select {
	case result := <-waiterDone:
		if result.err != nil || result.value != 11 {
			t.Fatalf("waiter result = (%d, %#v), want (11, nil)", result.value, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not receive work after leader deadline")
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want one shared fetch", got)
	}
}

func TestCacheAbandonedFlightCompletesAndWarmsCache(t *testing.T) {
	c := newCache[int](4, 1<<20, make(chan struct{}, 1))
	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(fetchCtx context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		close(started)
		select {
		case <-release:
			return 9, time.Hour, nil
		case <-fetchCtx.Done():
			return 0, 0, contextAppError(fetchCtx)
		}
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	firstDone := make(chan *AppError, 1)
	go func() {
		_, appErr := c.get(requestCtx, "abandoned", nil, fetch)
		firstDone <- appErr
	}()
	<-started
	cancelRequest()
	if appErr := <-firstDone; appErr == nil || appErr.Status != 499 {
		t.Fatalf("cancelled request error = %#v, want 499", appErr)
	}

	close(release)
	waitUntil(t, time.Second, func() bool {
		c.flightMu.Lock()
		defer c.flightMu.Unlock()
		return len(c.flight) == 0
	})

	value, appErr := c.get(context.Background(), "abandoned", nil, fetch)
	if appErr != nil || value != 9 {
		t.Fatalf("warmed get = (%d, %#v), want (9, nil)", value, appErr)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want one completed shared fetch", got)
	}
}

func TestCacheSemaphoreWaitHonorsSharedWorkDeadline(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	c := newCache[int](4, 1<<20, slots)
	c.workTimeout = 40 * time.Millisecond
	var fetches atomic.Int32

	_, appErr := c.get(context.Background(), "blocked", nil, func(context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		return 1, time.Hour, nil
	})
	if appErr == nil || appErr.Status != http.StatusGatewayTimeout || !appErr.Ephemeral {
		t.Fatalf("blocked request error = %#v, want ephemeral 504", appErr)
	}
	waitUntil(t, time.Second, func() bool {
		c.flightMu.Lock()
		defer c.flightMu.Unlock()
		return len(c.flight) == 0
	})
	<-slots
	if got := fetches.Load(); got != 0 {
		t.Fatalf("origin fetches = %d, want 0", got)
	}
}

func TestCacheNeverStoresStatus499(t *testing.T) {
	c := newCache[int](4, 1<<20, make(chan struct{}, 1))
	var fetches atomic.Int32
	fetch := func(context.Context) (int, time.Duration, *AppError) {
		fetches.Add(1)
		return 0, time.Hour, igErr(499, errorCodeConnection, "cancelled")
	}
	for i := 0; i < 2; i++ {
		_, appErr := c.get(context.Background(), "cancelled", nil, fetch)
		if appErr == nil || appErr.Status != 499 {
			t.Fatalf("get #%d error = %#v, want 499", i+1, appErr)
		}
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("origin fetches = %d, want 2 because 499 must not be cached", got)
	}
}

func TestPersistentCacheRejectsInvalidRemoteModel(t *testing.T) {
	invalid := Post{Shortcode: "Ab_12", Username: "valid_user"}
	body, err := json.Marshal(cachePayload[Post]{Value: invalid})
	if err != nil {
		t.Fatal(err)
	}
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("X-Cache-Expires", unixMillis(time.Now().Add(time.Hour)))
			_, _ = w.Write(body)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		puts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newPersistentCache[Post](srv.URL, "post", make(chan struct{}, 1), 4096)
	var fetches atomic.Int32
	valid := validTestPost("Ab_12", "fresh caption")
	got, appErr := c.get(context.Background(), "Ab_12", nil, func(context.Context) (Post, time.Duration, *AppError) {
		fetches.Add(1)
		return valid, time.Hour, nil
	})
	if appErr != nil || got.Caption != valid.Caption || len(got.Attachments) != 1 {
		t.Fatalf("get() = (%#v, %#v), want valid fetched post", got, appErr)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want 1 after rejecting corrupt KV payload", got)
	}
	waitUntil(t, time.Second, func() bool { return puts.Load() == 1 })
}

func TestPersistentCacheWriteFailureDoesNotReplaceSuccessfulFetch(t *testing.T) {
	var puts atomic.Int32
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			puts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1), 4096)
	value, appErr := c.get(context.Background(), "marker-fails", nil,
		func(context.Context) (int, time.Duration, *AppError) {
			fetches.Add(1)
			return 42, time.Hour, nil
		})
	if value != 42 || appErr != nil {
		t.Fatalf("cache write failure = (%d, %#v), want successful origin value", value, appErr)
	}
	again, appErr := c.get(context.Background(), "marker-fails", nil,
		func(context.Context) (int, time.Duration, *AppError) {
			fetches.Add(1)
			return 99, time.Hour, nil
		})
	if again != 42 || appErr != nil || fetches.Load() != 1 {
		t.Fatalf("local reuse = (%d, %#v), origin fetches=%d; want (42, nil), 1", again, appErr, fetches.Load())
	}
	waitUntil(t, time.Second, func() bool { return puts.Load() == 1 })
}

func TestPersistentZeroTTLSuccessReturnsWithoutPersistence(t *testing.T) {
	var puts atomic.Int32
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			puts.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newPersistentCache[int](srv.URL, "post", make(chan struct{}, 1), 4096)
	for want := int32(1); want <= 2; want++ {
		value, appErr := c.get(context.Background(), "zero-ttl", nil,
			func(context.Context) (int, time.Duration, *AppError) {
				return int(fetches.Add(1)), 0, nil
			})
		if appErr != nil || value != int(want) {
			t.Fatalf("zero-TTL get #%d = (%d, %#v), want successful value", want, value, appErr)
		}
	}
	if puts.Load() != 0 {
		t.Fatalf("zero-TTL persistence puts=%d, want 0", puts.Load())
	}
}

func TestPersistentCacheDetachesStringsAndCapsEntries(t *testing.T) {
	c := newPersistentCache[Post]("", "post", make(chan struct{}, 1), 1<<20)
	c.maxEntries = 2

	backing := strings.Repeat("x", 1<<20)
	caption := backing[:64]
	first := validTestPost("First_1", caption)
	got, appErr := c.get(context.Background(), first.Shortcode, nil, func(context.Context) (Post, time.Duration, *AppError) {
		return first, time.Hour, nil
	})
	if appErr != nil {
		t.Fatalf("first get error = %#v", appErr)
	}
	if unsafe.StringData(got.Caption) == unsafe.StringData(caption) {
		t.Fatal("cached caption still aliases the large upstream response string")
	}

	for _, shortcode := range []string{"Second_2", "Third_3"} {
		post := validTestPost(shortcode, shortcode)
		if _, appErr := c.get(context.Background(), shortcode, nil, func(context.Context) (Post, time.Duration, *AppError) {
			return post, time.Hour, nil
		}); appErr != nil {
			t.Fatalf("get(%q) error = %#v", shortcode, appErr)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := len(c.entries); got != 2 {
		t.Fatalf("local entries = %d, want hard cap of 2", got)
	}
	if _, exists := c.entries[first.Shortcode]; exists {
		t.Fatal("oldest entry was not evicted at the entry-count cap")
	}
	if c.usedBytes > c.maxBytes {
		t.Fatalf("usedBytes = %d, exceeds maxBytes = %d", c.usedBytes, c.maxBytes)
	}
}

func validTestPost(shortcode, caption string) Post {
	return Post{
		Shortcode: shortcode,
		Username:  "valid_user",
		Caption:   caption,
		Attachments: []Attachment{{
			Kind:      "image",
			URL:       "https://scontent.cdninstagram.com/media.jpg",
			Thumbnail: "https://scontent.cdninstagram.com/thumb.jpg",
			Width:     1080,
			Height:    1080,
		}},
	}
}

func unixMillis(value time.Time) string {
	return strconv.FormatInt(value.UnixMilli(), 10)
}
