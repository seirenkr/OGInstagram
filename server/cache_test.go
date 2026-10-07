package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	synctest.Test(t, func(t *testing.T) {
		slots := make(chan struct{}, 2)
		c1 := newPersistentCache[int](nil, "test", slots, 1<<20)
		c2 := newPersistentCache[int](nil, "test", slots, 1<<20)
		started := make(chan string, 3)
		release := make(chan struct{})
		defer close(release) // unblock fetchers if an assertion fails; synctest panics on leaked goroutines
		done := make(chan struct{}, 3)

		launch := func(c *cache[int], key string) {
			go func() {
				c.get(context.Background(), key, nil, func(context.Context) (int, time.Duration, bool, *AppError) {
					started <- key
					<-release
					return 1, time.Hour, true, nil
				})
				done <- struct{}{}
			}()
		}
		launch(c1, "a")
		launch(c1, "b")
		launch(c2, "c")

		<-started
		<-started
		synctest.Wait()
		select {
		case key := <-started:
			t.Fatalf("third fetch %q started before a slot was released", key)
		default:
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

func TestStatusRequestWarmsLocalEntryUsedByOffload(t *testing.T) {
	store := newTestStore(t)

	directCalls := 0
	slots := make(chan struct{}, 1)
	stubDefaultTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		directCalls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(simpleEmbedImagePage))}, nil
	}))
	a := &App{
		cfg:           Config{BaseURL: "https://example.test"},
		pool:          &SessionPool{}, // no proxy: GraphQL fails fast, captioned serves
		offloadSigner: mustOffloadSigner(testOffloadSigningKeys),
		posts:         newPersistentCache[Post](store, "post", slots, localPostCacheBytes),
	}

	code := statusSnowcode("p", "DaD8phTyclR", 0, false, false)
	status := serveApp(t, a, httptest.NewRequest(http.MethodGet, "https://example.test/users/instagram/statuses/"+code, nil))
	if status.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status.status)
	}

	if _, stored := a.posts.localGet("DaD8phTyclR"); !stored {
		t.Fatal("captioned result did not warm the local model cache")
	}

	offload := httptest.NewRequest(http.MethodGet, "https://example.test/offload/DaD8phTyclR/1", nil)
	media := serveApp(t, a, offload)
	if media.status != http.StatusFound || directCalls != 1 {
		t.Fatalf("offload = %d, Instagram fetches = %d; want 302 and one fetch", media.status, directCalls)
	}
}

func TestEdgeAuthorizedDirectOffloadMayFetchOrigin(t *testing.T) {
	var originFetches atomic.Int32
	postCache := newPersistentCache[Post](nil, "test", make(chan struct{}, 1), 1<<20)
	stubDefaultTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		originFetches.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(simpleEmbedImagePage)),
		}, nil
	}))
	a := &App{
		pool:  &SessionPool{},
		posts: postCache,
	}
	req := httptest.NewRequest(http.MethodGet, "https://example.test/offload/DaD8phTyclR/1", nil)
	result := serveApp(t, a, req)
	if result.status != http.StatusFound {
		t.Fatalf("authorized offload status = %d, want 302", result.status)
	}
	if got := originFetches.Load(); got != 1 {
		t.Fatalf("origin fetches = %d, want 1", got)
	}
}

func TestOffloadRejectsNonCanonicalSuffixBeforeLookup(t *testing.T) {
	for _, path := range []string{
		"/offload/Ab_12/anything",
		"/offload/Ab_12/0",
		"/offload/Ab_12/01",
		"/offload/Ab_12/1.mp4",
		"/offload/Ab_12/51",
		"/offload/@User.Name/01",
		"/offload/@User.Name/7",
		"/offload/@bad!/avatar",
	} {
		if _, ok := canonicalOffloadPath(&url.URL{Path: path}); ok {
			t.Errorf("%s was canonicalized", path)
		}
		if _, ok := resolveGatewayRoute(httptest.NewRequest(http.MethodGet, "https://example.test"+path, nil)); ok {
			t.Errorf("%s was routed, want 404 before any lookup", path)
		}
	}
	for _, tt := range []struct{ path, want string }{
		{"/offload/Ab_12/1/", ""},
		{"/offload//Ab_12/1", ""},
		{"/offload/story/Alice/123/avatar", "/offload/story/alice/123/avatar"},
		{"/offload/@User.Name/6", "/offload/@user.name/6"},
	} {
		if got, ok := canonicalOffloadPath(&url.URL{Path: tt.path}); got != tt.want || ok != (tt.want != "") {
			t.Errorf("canonicalOffloadPath(%s) = %q, %v; want %q", tt.path, got, ok, tt.want)
		}
	}
}

func TestCacheLeaderCancellationDoesNotPoisonSharedWork(t *testing.T) {
	c := newPersistentCache[int](nil, "test", make(chan struct{}, 1), 1<<20)
	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(fetchCtx context.Context) (int, time.Duration, bool, *AppError) {
		fetches.Add(1)
		close(started)
		select {
		case <-release:
			return 7, time.Hour, true, nil
		case <-fetchCtx.Done():
			return 0, 0, false, contextAppError(fetchCtx)
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

	c := newPersistentCache[int](nil, "test", make(chan struct{}, 1), 1<<20)
	c.workTimeout = time.Second
	started := make(chan *AppError, 1)
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(fetchCtx context.Context) (int, time.Duration, bool, *AppError) {
		fetches.Add(1)
		if got := fetchCtx.Value(contextKey{}); got != contextValue {
			appErr := ephemeralErr(500, errorCodeUpstream, "request context value was not preserved")
			started <- appErr
			return 0, 0, false, appErr
		}
		deadline, ok := fetchCtx.Deadline()
		if !ok || time.Until(deadline) < 500*time.Millisecond {
			appErr := ephemeralErr(500, errorCodeUpstream, "shared work inherited the leader deadline")
			started <- appErr
			return 0, 0, false, appErr
		}
		started <- nil
		select {
		case <-release:
			return 11, time.Hour, true, nil
		case <-fetchCtx.Done():
			return 0, 0, false, contextAppError(fetchCtx)
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
	c := newPersistentCache[int](nil, "test", make(chan struct{}, 1), 1<<20)
	started := make(chan struct{})
	release := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(fetchCtx context.Context) (int, time.Duration, bool, *AppError) {
		fetches.Add(1)
		close(started)
		select {
		case <-release:
			return 9, time.Hour, true, nil
		case <-fetchCtx.Done():
			return 0, 0, false, contextAppError(fetchCtx)
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
	c := newPersistentCache[int](nil, "test", slots, 1<<20)
	c.workTimeout = 40 * time.Millisecond
	var fetches atomic.Int32

	_, appErr := c.get(context.Background(), "blocked", nil, func(context.Context) (int, time.Duration, bool, *AppError) {
		fetches.Add(1)
		return 1, time.Hour, true, nil
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
	c := newPersistentCache[int](nil, "test", make(chan struct{}, 1), 1<<20)
	var fetches atomic.Int32
	fetch := func(context.Context) (int, time.Duration, bool, *AppError) {
		fetches.Add(1)
		return 0, time.Hour, true, igErr(499, errorCodeConnection, "cancelled")
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

func TestPersistentCacheDetachesStringsAndCapsEntries(t *testing.T) {
	c := newPersistentCache[Post](nil, "post", make(chan struct{}, 1), 1<<20)
	c.maxEntries = 2

	backing := strings.Repeat("x", 1<<20)
	caption := backing[:64]
	first := validTestPost("First_1", caption)
	got, appErr := c.get(context.Background(), first.Shortcode, nil, func(context.Context) (Post, time.Duration, bool, *AppError) {
		return first, time.Hour, true, nil
	})
	if appErr != nil {
		t.Fatalf("first get error = %#v", appErr)
	}
	if unsafe.StringData(got.Caption) == unsafe.StringData(caption) {
		t.Fatal("cached caption still aliases the large upstream response string")
	}

	for _, shortcode := range []string{"Second_2", "Third_3"} {
		post := validTestPost(shortcode, shortcode)
		if _, appErr := c.get(context.Background(), shortcode, nil, func(context.Context) (Post, time.Duration, bool, *AppError) {
			return post, time.Hour, true, nil
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
