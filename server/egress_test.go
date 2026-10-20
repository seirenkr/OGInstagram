package main

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPKShortcodeRoundTrips(t *testing.T) {
	if got := pkShortcode(4011387627600768522); got != "DerT8tHim4K" {
		t.Fatalf("pkShortcode = %q, want DerT8tHim4K", got)
	}
	for _, pk := range []uint64{1, 63, 64, 1 << 40, 18446744073709551615} {
		if got := shortcodePK(pkShortcode(pk)); got == nil || got.Uint64() != pk {
			t.Fatalf("pk %d round-tripped to %v", pk, got)
		}
	}
}

func storyBody(productType, pk, username string) string {
	return `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"pk":"` + pk + `","code":"DerT8tHim4K","product_type":"` + productType +
		`","media_type":1,"taken_at":1790000000,"user":{"username":"` + username + `","full_name":"Name","is_verified":true},` +
		`"image_versions2":{"candidates":[{"url":"https://scontent.cdninstagram.com/s.jpg","width":1080,"height":1920}]}}]}}}`
}

func TestParseGraphQLStoryChecksTheURL(t *testing.T) {
	story, err := parseGraphQLStory("some_user", "4011387627600768522", storyBody("story", "4011387627600768522", "Some_User"))
	if err != nil || story.Username != "Some_User" || !story.IsVerified || story.Media.Width != 1080 || story.CreatedAt.IsZero() {
		t.Fatalf("story = %+v, %v; want the owner's story", story, err)
	}
	for name, body := range map[string]string{
		"other owner":          storyBody("story", "4011387627600768522", "someone_else"),
		"owner differing in _": storyBody("story", "4011387627600768522", "some__user"),
		"feed post":            storyBody("feed", "4011387627600768522", "some_user"),
		"other id":             storyBody("story", "4011387627600768523", "some_user"),
	} {
		if _, err := parseGraphQLStory("some_user", "4011387627600768522", body); err == nil || err.Code != errorCodeMediaNotFound || !err.Final {
			t.Fatalf("%s: err = %#v, want a final media_not_found", name, err)
		}
	}
}

// v6 answers with fn on ipv6Strikes+1 sessions; both pools count their calls,
// and DC always succeeds.
func fallbackApp(fn func() (int, string)) (a *App, v6Calls, dcCalls *atomic.Int32) {
	return fallbackAppReq(func(*http.Request) (int, string) { return fn() })
}

// fallbackAppReq is fallbackApp with the request in hand; status 0 hangs until
// the request is cancelled, like a dead path.
func fallbackAppReq(fn func(*http.Request) (int, string)) (a *App, v6Calls, dcCalls *atomic.Int32) {
	v6Calls, dcCalls = new(atomic.Int32), new(atomic.Int32)
	reply := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
	}
	v6 := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		v6Calls.Add(1)
		status, body := fn(r)
		if status == 0 {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return reply(status, body), nil
	})}
	dc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		dcCalls.Add(1)
		return reply(200, `{"data":{"ok":true}}`), nil
	})}
	v6Pool := &SessionPool{proxyURL: func() string { return "http://x@egress" }}
	for i := range ipv6Strikes + 1 {
		v6Pool.sessions = append(v6Pool.sessions, &Session{name: "v6-" + string(rune('a'+i)), client: v6})
	}
	return &App{cfg: Config{EgressPercent: 100}, pool: &SessionPool{sessions: []*Session{{name: "us-1", client: dc}}}, v6: v6Pool}, v6Calls, dcCalls
}

func TestFetchViaProxyFallsBackFromIPv6(t *testing.T) {
	throttled := func() (int, string) { return 200, `{"errors":[{"code":1675004,"message":"Rate limit exceeded"}]}` }
	a, v6Calls, dcCalls := fallbackApp(throttled)
	for i := 1; i <= ipv6Strikes+1; i++ {
		if _, body, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"ok"`) {
			t.Fatalf("request %d: %q, %v; want the DC answer after an IPv6 throttle", i, body, err)
		}
	}
	// Three throttles reached IPv6 and paused it; the fourth went straight to DC.
	if v6Calls.Load() != ipv6Strikes || dcCalls.Load() != ipv6Strikes+1 {
		t.Fatalf("IPv6 calls = %d, DC calls = %d; want %d and %d", v6Calls.Load(), dcCalls.Load(), ipv6Strikes, ipv6Strikes+1)
	}
	if a.v6Gate("post").allow() {
		t.Fatal("IPv6 still allowed after repeated throttles")
	}
	// A request that started before the pause and fails after it does not extend it.
	g := a.v6Gate("post")
	until := g.until
	g.fail("post")
	if g.until != until {
		t.Fatal("a straggler's failure extended the pause")
	}

	// Muted licensed music gets one DC recheck.
	muted := `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"clips_metadata":{"music_info":{"music_consumption_info":{"should_mute_audio":true}}}}]}}}`
	a, _, dcCalls = fallbackApp(func() (int, string) { return 200, muted })
	if _, got, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(got, `"ok"`) || dcCalls.Load() != 1 {
		t.Fatalf("muted: body %q, err %v, DC calls %d; want the DC copy", got, err, dcCalls.Load())
	}
	// 1675030 comes back at once; hiddenPost races DC separately.
	a, _, dcCalls = fallbackApp(func() (int, string) { return 200, `{"data":null,"errors":[{"code":1675030}]}` })
	if _, got, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(got, "1675030") || dcCalls.Load() != 0 {
		t.Fatalf("hidden: body %q, err %v, DC calls %d; want the IPv6 answer as is", got, err, dcCalls.Load())
	}

	// Other answers about the content come back as is.
	a, _, dcCalls = fallbackApp(func() (int, string) { return 400, `{"message":"bad request","status":"fail"}` })
	if _, _, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err == nil || err.Code != errorCodeGraphQL || dcCalls.Load() != 0 {
		t.Fatalf("err %v, DC calls %d; want the IPv6 answer as is", err, dcCalls.Load())
	}

	// A short deadline leaves DC half of it: a hung IPv6 path still falls back.
	a, _, dcCalls = fallbackAppReq(func(*http.Request) (int, string) { return 0, "" })
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, body, err := a.fetchViaProxy(ctx, webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"ok"`) || dcCalls.Load() != 1 {
		t.Fatalf("body %q, err %v, DC calls %d; want DC within the caller's deadline", body, err, dcCalls.Load())
	}
}

func TestEgressRefusesOtherHosts(t *testing.T) {
	srv := httptest.NewServer(egressHandler([]net.IP{net.ParseIP("::1")}))
	defer srv.Close()
	for _, target := range []string{"example.com:443", "i.instagram.com:80", "169.254.169.254:80"} {
		req, _ := http.NewRequest(http.MethodConnect, srv.URL, nil)
		req.Host = target
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s = %d, want 403", target, resp.StatusCode)
		}
	}
	if got := egressSession("Basic " + base64.StdEncoding.EncodeToString([]byte("abc123:"))); got != "abc123" {
		t.Fatalf("egressSession = %q, want abc123", got)
	}
}

func TestStory1675030IsFinalUntilThePathLooksClosed(t *testing.T) {
	var d storySideDoor
	for i := range 100 {
		if d.closed(i%12 == 0) { // ~8%: private and deleted stories
			t.Fatal("the usual share of 1675030 marked the path closed")
		}
	}
	closed := false
	for range 60 {
		closed = d.closed(true)
	}
	if !closed {
		t.Fatal("a majority of 1675030 did not mark the path closed")
	}

	helpers := new(atomic.Int32)
	previous := externalHelperStoryImpl
	t.Cleanup(func() { externalHelperStoryImpl = previous })
	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		helpers.Add(1)
		return Story{}, igErr(http.StatusNotFound, errorCodeMediaNotFound, "story not available")
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"code":1675030}]}`)), nil
	})}
	a := newApp(Config{}, &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}, offloadSigner{})
	if _, err := a.getStory(context.Background(), "someone", "4011387627600768522", nil); err == nil || err.Code != errorCodeMediaNotFound || helpers.Load() != 0 {
		t.Fatalf("err = %v, helper calls = %d; want a final miss without the helper", err, helpers.Load())
	}
}
