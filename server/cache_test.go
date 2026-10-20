package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// trustedEmbedImagePage is simpleEmbedImagePage with media on an Instagram CDN host,
// for tests whose parsed post must pass cached-model validation.
var trustedEmbedImagePage = strings.ReplaceAll(simpleEmbedImagePage, "https://cdn/", "https://scontent.cdninstagram.com/")

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
	post.Attachments[0].URL = "https://evil.example/video.mp4"
	if _, valid := cloneAndValidateCacheValue("Ab_12", post); valid {
		t.Fatal("non-CDN media URL accepted into the cache")
	}
}

// graphQLPool answers the post query with an image post whose media URL is
// https://scontent.cdninstagram.com/big.jpg?oe=1, counting the calls.
func graphQLPool(calls *atomic.Int32) *SessionPool {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return mediaTestResponse(r, http.StatusOK, nil, []byte(`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"DaD8phTyclR",`+
			`"user":{"username":"instagram"},"display_url":"https://scontent.cdninstagram.com/big.jpg?oe=1","media_type":1}]}}}`)), nil
	})}
	return &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}
}

func TestStatusRequestWarmsLocalEntryUsedByOffload(t *testing.T) {
	store := newTestStore(t)

	var calls atomic.Int32
	a := newApp(Config{BaseURL: "https://example.test", Store: store}, graphQLPool(&calls), mustOffloadSigner(testOffloadSigningKeys))

	note := httptest.NewRequest(http.MethodGet, "https://example.test/p/DaD8phTyclR", nil)
	note.Header.Set("Accept", asMediaType)
	status := serveApp(t, a, note)
	if status.status != http.StatusOK || status.headers["Content-Type"] != asMediaType {
		t.Fatalf("status = %d, want 200", status.status)
	}

	if _, stored := a.posts.localGet("DaD8phTyclR"); !stored {
		t.Fatal("the GraphQL result did not warm the local model cache")
	}

	offload := httptest.NewRequest(http.MethodGet, "https://example.test/offload/DaD8phTyclR/1", nil)
	media := serveApp(t, a, offload)
	if media.status != http.StatusFound || calls.Load() != 1 {
		t.Fatalf("offload = %d, Instagram fetches = %d; want 302 and one fetch", media.status, calls.Load())
	}
}

func TestOffloadMissFetchesOrigin(t *testing.T) {
	var originFetches atomic.Int32
	a := newApp(Config{}, graphQLPool(&originFetches), mustOffloadSigner(testOffloadSigningKeys))
	req := httptest.NewRequest(http.MethodGet, "https://example.test/offload/DaD8phTyclR/1", nil)
	result := serveApp(t, a, req)
	if want := "https://scontent.cdninstagram.com/big.jpg?oe=1"; result.status != http.StatusFound || result.headers["Location"] != want {
		t.Fatalf("offload = %d to %q, want 302 to %q", result.status, result.headers["Location"], want)
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

func TestCacheFlightOutlivesItsLeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type contextKey struct{}
		c := newPersistentCache[int](nil, "test", make(chan struct{}, 1), 1<<20)
		release := make(chan struct{})
		defer close(release)
		var fetches atomic.Int32
		fetch := func(fetchCtx context.Context) (int, time.Duration, bool, *AppError) {
			fetches.Add(1)
			if deadline, ok := fetchCtx.Deadline(); fetchCtx.Value(contextKey{}) != "request-id" || !ok || time.Until(deadline) != c.workTimeout {
				t.Error("flight lost the leader's context value or inherited its deadline")
			}
			<-release
			return 7, time.Hour, true, nil
		}

		leaderCtx, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey{}, "request-id"), 100*time.Millisecond)
		defer cancel()
		leader := make(chan *AppError, 1)
		go func() {
			_, appErr := c.get(leaderCtx, "shared", nil, fetch)
			leader <- appErr
		}()
		synctest.Wait()
		waiter := make(chan int, 1)
		go func() {
			value, appErr := c.get(context.Background(), "shared", nil, fetch)
			if appErr != nil {
				t.Errorf("waiter error = %#v", appErr)
			}
			waiter <- value
		}()
		synctest.Wait()
		if appErr := <-leader; appErr == nil || appErr.Status != http.StatusGatewayTimeout {
			t.Fatalf("leader error = %#v, want 504", appErr)
		}
		release <- struct{}{}
		if value := <-waiter; value != 7 {
			t.Fatalf("waiter value = %d, want 7", value)
		}
		if value, appErr := c.get(context.Background(), "shared", nil, fetch); appErr != nil || value != 7 || fetches.Load() != 1 {
			t.Fatalf("cached get = (%d, %#v) after %d fetches, want (7, nil) after 1", value, appErr, fetches.Load())
		}
	})
}

func TestCacheSemaphoreWaitHonorsSharedWorkDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		synctest.Wait()
		<-slots
		if got := fetches.Load(); got != 0 {
			t.Fatalf("origin fetches = %d, want 0", got)
		}
	})
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
	c := newPersistentCache[Post](nil, "post", make(chan struct{}, 1), 1<<30)

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

	for i := range persistentLocalEntryCap {
		shortcode := "Post_" + strconv.Itoa(i)
		c.storeLocal(shortcode, &cacheEntry[Post]{value: validTestPost(shortcode, shortcode), expiresAt: time.Now().Add(time.Hour)})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := len(c.entries); got != persistentLocalEntryCap {
		t.Fatalf("local entries = %d, want hard cap of %d", got, persistentLocalEntryCap)
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

func TestLocalCacheByteAccounting(t *testing.T) {
	c := newPersistentCache[string](nil, "test", nil, 0)
	entry := func(value string) *cacheEntry[string] {
		return &cacheEntry[string]{value: value, expiresAt: time.Now().Add(time.Hour)}
	}
	c.maxBytes = 3*cacheEntryMemorySize("a", entry("1")) + 8
	check := func(step string, want ...string) {
		t.Helper()
		c.mu.Lock()
		defer c.mu.Unlock()
		var sum int64
		for _, e := range c.entries {
			sum += e.size
		}
		if c.usedBytes != sum || c.usedBytes > c.maxBytes {
			t.Fatalf("%s: usedBytes = %d, entry sizes = %d, maxBytes = %d", step, c.usedBytes, sum, c.maxBytes)
		}
		if len(c.entries) != len(want) || len(c.order) != len(want) {
			t.Fatalf("%s: entries = %d, order = %v, want %v", step, len(c.entries), c.order, want)
		}
		for _, key := range want {
			if _, ok := c.entries[key]; !ok {
				t.Fatalf("%s: %q missing, order = %v", step, key, c.order)
			}
		}
	}

	for _, key := range []string{"a", "b", "c"} {
		c.storeLocal(key, entry("1"))
	}
	check("fill", "a", "b", "c")
	c.storeLocal("d", entry("1"))
	check("evict oldest", "b", "c", "d")
	c.storeLocal("c", entry("1234567890"))
	check("overwrite grows", "c", "d")
	c.mu.Lock()
	c.entries["d"].expiresAt = time.Now().Add(-time.Second)
	c.mu.Unlock()
	if _, ok := c.localGet("d"); ok {
		t.Fatal("expired entry returned")
	}
	check("expire", "c")
	c.storeLocal("f", entry(strings.Repeat("x", int(c.maxBytes))))
	check("oversize ignored", "c")

	gone := &cacheEntry[string]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: time.Now().Add(time.Hour)}
	for i := range errorLocalEntryCap + 1 {
		c.storeLocal("gone"+strconv.Itoa(i), gone)
	}
	check("errors never evict values", "c")
	if e, ok := c.localGet("gone1"); !ok || e.err == nil || len(c.errEntries) != errorLocalEntryCap {
		t.Fatalf("error entries = %d, want cap %d with gone1 kept", len(c.errEntries), errorLocalEntryCap)
	}
	c.storeLocal("gone1", entry("1"))
	check("value replaces error", "c", "gone1")
	if len(c.errEntries) != errorLocalEntryCap-1 || len(c.errOrder) != errorLocalEntryCap-1 {
		t.Fatalf("error entries = %d, order = %d after a value replaced one", len(c.errEntries), len(c.errOrder))
	}
}

func TestPutAfterFlightWaitsForRunningLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newPersistentCache[int](nil, "test", nil, 1<<20)
		call := &flightCall[int]{done: make(chan struct{})}
		c.flight["k"] = call
		go c.putAfterFlight(context.Background(), "k", 2, time.Hour)
		synctest.Wait()
		if _, ok := c.localGet("k"); ok {
			t.Fatal("put ran before the running lookup finished")
		}
		c.storeLocal("k", &cacheEntry[int]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: time.Now().Add(time.Hour)})
		close(call.done)
		synctest.Wait()
		if e, ok := c.localGet("k"); !ok || e.err != nil || e.value != 2 {
			t.Fatalf("entry after lookup = %+v, want the put value", e)
		}
	})
}

func TestCacheFetchPanicBecomesUncachedError(t *testing.T) {
	c := newPersistentCache[int](nil, "test", make(chan struct{}, 1), 1<<20)
	var fetches atomic.Int32
	for range 2 {
		_, appErr := c.get(context.Background(), "panic", nil, func(context.Context) (int, time.Duration, bool, *AppError) {
			fetches.Add(1)
			panic("parser bug")
		})
		if appErr == nil || appErr.Status != http.StatusBadGateway || !appErr.Ephemeral {
			t.Fatalf("panicking fetch error = %#v, want ephemeral 502", appErr)
		}
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2 because a panic is not cached", got)
	}
}
