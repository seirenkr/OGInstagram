package main

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	return `{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"pk":"` + pk + `","code":"DerT8tHim4K","product_type":"` + productType +
		`","media_type":1,"taken_at":1790000000,"user":{"username":"` + username + `","full_name":"Name","is_verified":true},` +
		`"image_versions2":{"candidates":[{"url":"https://scontent.cdninstagram.com/s.jpg","width":1080,"height":1920}]}}}}}`
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
// and DC always succeeds. Status 0 hangs until the request is cancelled, like a dead path.
func fallbackApp(fn func(*http.Request) (int, string)) (a *App, v6Calls, dcCalls *atomic.Int32) {
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
	v6Pool := &SessionPool{
		addrs:     newEgressAddrs(egressPoolSize, nil),
		newClient: func(*url.URL) *http.Client { return v6 },
	}
	for i := range ipv6Strikes*(ipv6Retries+1) + 1 {
		_, addr, _ := v6Pool.nextProxy(-1)
		v6Pool.sessions = append(v6Pool.sessions, &Session{name: "v6-" + string(rune('a'+i)), addr: addr, client: v6})
	}
	return &App{cfg: Config{EgressPercent: 100}, pool: &SessionPool{sessions: []*Session{{name: "us-1", client: dc}}}, v6: v6Pool}, v6Calls, dcCalls
}

func TestDCGatePausesAnOperationEveryExitRefuses(t *testing.T) {
	var calls atomic.Int32
	var recovered atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `{"data":{"ok":true}}`
		if !recovered.Load() && strings.Contains(r.Header.Get("X-FB-Friendly-Name"), "PostRootContent") {
			body = `{"errors":[{"code":1675004,"message":"Rate limit exceeded"}]}`
		}
		return mediaTestResponse(r, 200, nil, []byte(body)), nil
	})}
	pool := &SessionPool{}
	// Rotation swaps in a real client, so every refused attempt needs its own session.
	for i := range ipv6Window + 2 {
		pool.sessions = append(pool.sessions, &Session{name: "us-" + strconv.Itoa(i), addr: -1, client: client})
	}
	a := &App{pool: pool}
	post := func() *AppError {
		_, _, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K"))
		return err
	}
	for i := range ipv6Window + 5 {
		if err := post(); err == nil || (i >= ipv6Window && !err.Ephemeral) {
			t.Fatalf("request %d: err = %#v; want a refusal, then the pause as an ephemeral error", i, err)
		}
	}
	if calls.Load() != ipv6Window {
		t.Fatalf("DC calls = %d; want the pause after %d refusals in a row", calls.Load(), ipv6Window)
	}
	if _, _, err := a.fetchViaProxy(context.Background(), loggedOutQuerySpec("profile", "Profile", profileRootDocID, nil)); err != nil {
		t.Fatalf("another operation failed while post was paused: %v", err)
	}
	recovered.Store(true)
	g := a.dcGate("post")
	g.mu.Lock()
	g.until = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if err := post(); err != nil || !g.until.IsZero() {
		t.Fatalf("probe after the pause: err = %v, gate = %+v; want the operation back", err, g)
	}
}

func TestV6GateResumesAfterPause(t *testing.T) {
	g := &v6Gate{}
	for range ipv6Strikes {
		g.fail("post")
	}
	if g.allow() {
		t.Fatal("gate open right after the pause began")
	}
	g.ok("post", time.Now().Add(-time.Minute)) // a straggler from before the pause
	if g.until.IsZero() {
		t.Fatal("a straggler's success ended the pause")
	}
	g.until = time.Now().Add(-time.Second)
	if !g.allow() {
		t.Fatal("no probe after the pause ended")
	}
	g.ok("post", time.Now()) // used to clear the held mutex and crash on Unlock
	if !g.allow() || !g.until.IsZero() || g.failures != 0 || g.pause != ipv6PauseMin/2 {
		t.Fatalf("gate not reset after a successful probe, or the pause not halved: %+v", g)
	}
	for range ipv6Window {
		g.ok("post", time.Now())
	}
	if g.pause != 0 {
		t.Fatalf("pause = %v after sustained healthy streak, want 0", g.pause)
	}
}

func TestThrottledIPv6TriesAnotherAddress(t *testing.T) {
	var n atomic.Int32
	a, v6Calls, dcCalls := fallbackApp(func(*http.Request) (int, string) {
		if n.Add(1) <= ipv6Retries {
			return 200, `{"errors":[{"code":1675004,"message":"Rate limit exceeded"}]}`
		}
		return 200, `{"data":{"v6":true}}`
	})
	if _, body, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"v6"`) || v6Calls.Load() != ipv6Retries+1 || dcCalls.Load() != 0 {
		t.Fatalf("body %q, err %v, IPv6 calls %d, DC calls %d; want the third address to answer", body, err, v6Calls.Load(), dcCalls.Load())
	}
}

func TestFailedEgressAddressSitsOut(t *testing.T) {
	usage := newEgressUsage(3)
	a1Book := newEgressAddrs(3, usage)
	a2Book := newEgressAddrs(3, usage)
	p1 := &SessionPool{addrs: a1Book}
	p2 := &SessionPool{addrs: a2Book}

	_, a1, rested1 := p1.nextProxy(-1)
	_, a2, rested2 := p2.nextProxy(-1)
	if !rested1 || !rested2 || a1 == a2 {
		t.Fatalf("initial picks = (%d,%v) and (%d,%v); want two distinct rested addresses", a1, rested1, a2, rested2)
	}
	a1Book.release(a1)
	a2Book.release(a2)

	base := time.Now()
	a1Book.down[0] = base.Add(20 * time.Minute)
	a1Book.down[1] = base.Add(10 * time.Minute)
	for range 10 {
		u, addr, rested := p1.nextProxy(-1)
		a1Book.release(addr)
		if !rested || addr != 2 || u.User.Username() != "a2" {
			t.Fatalf("picked %d (%s, rested=%v) with 0 and 1 sitting out", addr, u.User.Username(), rested)
		}
	}
	a2Book.down[1] = base.Add(10 * time.Minute)
	a2Book.down[2] = base.Add(20 * time.Minute)
	if _, addr, rested := p2.nextProxy(-1); !rested || addr != 0 {
		t.Fatalf("p2 blocked by p1 cooldown: got (%d, rested=%v), want (0, true)", addr, rested)
	} else {
		a2Book.release(addr)
	}
	a1Book.down[2] = base.Add(30 * time.Minute)
	if _, addr, rested := p1.nextProxy(-1); rested || addr != 1 {
		t.Fatalf("all down picked (%d, rested=%v), want (1, false)", addr, rested)
	} else {
		a1Book.release(addr)
	}
}

func TestShortSpecBudgetsPauseV6GateOnHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, dcCalls := fallbackApp(func(*http.Request) (int, string) { return 0, "" })
		a.v6Pages = a.v6
		var rotated atomic.Int32
		origNew := a.v6.newClient
		a.v6.newClient = func(u *url.URL) *http.Client {
			rotated.Add(1)
			return origNew(u)
		}
		for range ipv6Strikes + 2 {
			_, _ = a.fetchPostOG(context.Background(), "DerT8tHim4K")
		}
		if g := a.v6Gate("og_page"); g.allow() || rotated.Load() < ipv6Strikes {
			t.Fatalf("og_page gate=%+v rotated=%d dc=%d; want paused and rotated", g, rotated.Load(), dcCalls.Load())
		}
		for range ipv6Strikes + 2 {
			_ = a.mediaMissing(context.Background(), "DerT8tHim4K")
		}
		if g := a.v6Gate("ruling"); g.allow() {
			t.Fatalf("ruling gate=%+v; want paused after hangs", g)
		}
	})
}

func TestShortBudgetAndRetryHangHandling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, dcCalls := fallbackApp(func(*http.Request) (int, string) { return 0, "" })
		initial := a.v6.sessions[0].getClient()
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		if _, body, err := a.fetchViaProxy(ctx, webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"ok"`) || dcCalls.Load() != 1 {
			t.Fatalf("short budget: body=%q err=%v dc=%d", body, err, dcCalls.Load())
		}
		if g := a.v6Gate("post"); g.failures != 0 || !g.until.IsZero() {
			t.Fatalf("short budget timeout tripped v6Gate: %+v", g)
		}
		for _, s := range a.v6.sessions {
			if s.getClient() != initial {
				t.Fatal("short budget timeout rotated a healthy IPv6 session")
			}
		}

		var attempt atomic.Int32
		var rotated atomic.Int32
		a2, _, dcCalls2 := fallbackApp(func(r *http.Request) (int, string) {
			if attempt.Add(1) == 1 {
				time.Sleep(200 * time.Millisecond)
				return 200, `{"errors":[{"code":1675004,"message":"Rate limit exceeded"}]}`
			}
			<-r.Context().Done()
			return 0, ""
		})
		origNew := a2.v6.newClient
		a2.v6.newClient = func(u *url.URL) *http.Client {
			rotated.Add(1)
			return origNew(u)
		}
		if _, body, err := a2.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"ok"`) || dcCalls2.Load() != 1 {
			t.Fatalf("retry hang: body=%q err=%v dc=%d", body, err, dcCalls2.Load())
		}
		if rotated.Load() != 2 {
			t.Fatalf("rotations = %d, want 2 (429 attempt + hung retry)", rotated.Load())
		}
	})
}

func TestEgressSourceReadsTheIndex(t *testing.T) {
	pool := []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("2001:db8::2"), net.ParseIP("2001:db8::3")}
	basic := func(user string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":")) }
	if ip, ok := egressSource(pool, basic("a4")); !ok || !ip.Equal(pool[1]) {
		t.Fatalf("a4 = %v, %v; want %v", ip, ok, pool[1])
	}
	for _, auth := range []string{basic("x"), basic("a-1"), basic("a"), "Basic !!", ""} {
		if _, ok := egressSource(pool, auth); ok {
			t.Fatalf("%q accepted", auth)
		}
	}
}

func TestFetchViaProxyFallsBackFromIPv6(t *testing.T) {
	throttled := func(*http.Request) (int, string) {
		return 200, `{"errors":[{"code":1675004,"message":"Rate limit exceeded"}]}`
	}
	a, v6Calls, dcCalls := fallbackApp(throttled)
	for i := 1; i <= ipv6Strikes+1; i++ {
		if _, body, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"ok"`) {
			t.Fatalf("request %d: %q, %v; want the DC answer after an IPv6 throttle", i, body, err)
		}
	}
	// Each of three requests tried three addresses before DC and counted one
	// strike; that paused IPv6 and the fourth went straight to DC.
	if want := ipv6Strikes * (ipv6Retries + 1); v6Calls.Load() != int32(want) || dcCalls.Load() != ipv6Strikes+1 {
		t.Fatalf("IPv6 calls = %d, DC calls = %d; want %d and %d", v6Calls.Load(), dcCalls.Load(), want, ipv6Strikes+1)
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

	for _, muted := range []string{
		`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"clips_metadata":{"music_info":{"music_consumption_info":{"should_mute_audio":true}}}}}}}`,
		`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"clips_metadata":{"original_sound_info":{"should_mute_audio":true}}}}}}`,
	} {
		a, _, dcCalls = fallbackApp(func(*http.Request) (int, string) { return 200, muted })
		if _, got, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(got, `"ok"`) || dcCalls.Load() != 1 {
			t.Fatalf("muted: body %q, err %v, DC calls %d; want the DC copy", got, err, dcCalls.Load())
		}
	}
	for _, unmuted := range []string{
		`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"clips_metadata":{"original_sound_info":{"should_mute_audio":false,"consumption_info":{"should_mute_audio_reason":"","should_mute_audio_reason_type":""}}}}}}}`,
		`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"clips_metadata":{"original_sound_info":{"should_mute_audio":false,"consumption_info":{"should_mute_audio_reason_type":0}}}}}}}`,
		`{"data":{"xig_polaris_media":{"if_not_gated_logged_out":{"clips_metadata":{"original_sound_info":{"should_mute_audio":false,"consumption_info":{"should_mute_audio_reason_type":false}}}}}}}`,
	} {
		if mutedMusic(unmuted) {
			t.Fatalf("mutedMusic falsely matched default reason type: %s", unmuted)
		}
	}
	// A withheld post comes back at once; hiddenPost races DC separately.
	a, _, dcCalls = fallbackApp(func(*http.Request) (int, string) { return 200, `{"data":{"xig_polaris_media":null}}` })
	if _, got, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(got, "xig_polaris_media") || dcCalls.Load() != 0 {
		t.Fatalf("hidden: body %q, err %v, DC calls %d; want the IPv6 answer as is", got, err, dcCalls.Load())
	}

	// Other answers about the content come back as is.
	a, _, dcCalls = fallbackApp(func(*http.Request) (int, string) { return 400, `{"message":"bad request","status":"fail"}` })
	if _, _, err := a.fetchViaProxy(context.Background(), webLoggedOutSpec("DerT8tHim4K")); err == nil || err.Code != errorCodeGraphQL || dcCalls.Load() != 0 {
		t.Fatalf("err %v, DC calls %d; want the IPv6 answer as is", err, dcCalls.Load())
	}

	// A short deadline leaves DC half of it: a hung IPv6 path still falls back.
	a, _, dcCalls = fallbackApp(func(*http.Request) (int, string) { return 0, "" })
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, body, err := a.fetchViaProxy(ctx, webLoggedOutSpec("DerT8tHim4K")); err != nil || !strings.Contains(body, `"ok"`) || dcCalls.Load() != 1 {
		t.Fatalf("body %q, err %v, DC calls %d; want DC within the caller's deadline", body, err, dcCalls.Load())
	}
}

func TestEgressRefusesOtherHosts(t *testing.T) {
	srv := httptest.NewServer(egressHandler([]net.IP{net.ParseIP("::1")}, nil))
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
}

func TestWithheldStoryIsFinalUntilThePathLooksClosed(t *testing.T) {
	var d storySideDoor
	for i := range 100 {
		if d.closed(i%12 == 0) { // ~8%: private and deleted stories
			t.Fatal("the usual share of withheld stories marked the path closed")
		}
	}
	closed := false
	for range 60 {
		closed = d.closed(true)
	}
	if !closed {
		t.Fatal("a majority of withheld stories did not mark the path closed")
	}

	helpers := new(atomic.Int32)
	previous := externalHelperStoryImpl
	t.Cleanup(func() { externalHelperStoryImpl = previous })
	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		helpers.Add(1)
		return Story{}, igErr(http.StatusNotFound, errorCodeMediaNotFound, "story not available")
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mediaTestResponse(r, 200, nil, []byte(`{"data":{"xig_polaris_media":null}}`)), nil
	})}
	a := newApp(Config{}, &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}, offloadSigner{})
	if _, err := a.getStory(context.Background(), "someone", "4011387627600768522", nil); err == nil || err.Code != errorCodeMediaNotFound || helpers.Load() != 0 {
		t.Fatalf("err = %v, helper calls = %d; want a final miss without the helper", err, helpers.Load())
	}
}
