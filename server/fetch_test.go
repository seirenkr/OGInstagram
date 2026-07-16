package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestShortcodeTime(t *testing.T) {

	want := time.Date(2019, 1, 4, 17, 5, 45, 106e6, time.UTC)
	if got := shortcodeTime("BsOGulcndj-"); !got.Equal(want) {
		t.Errorf("shortcodeTime(BsOGulcndj-) = %v, want %v", got, want)
	}
	for _, sc := range []string{"", "has space", "AAAAAAAAAAAAAAAAAAAAAAAA"} {
		if got := shortcodeTime(sc); !got.IsZero() {
			t.Errorf("shortcodeTime(%q) = %v, want zero", sc, got)
		}
	}
}

func TestBuildEmbedHTMLProbesOnlySelectedVideo(t *testing.T) {
	requests := make(chan string, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if strings.Contains(r.URL.Path, "big") {
			w.Header().Set("Content-Length", strconv.FormatInt(int64(maxInlineVideoBytes)+1, 10))
		} else {
			w.Header().Set("Content-Length", "1")
		}
	}))
	defer ts.Close()
	a := &App{
		cfg:        Config{},
		direct:     ts.Client(),
		videoSizes: newCache[int64](3, make(chan struct{}, 1)),
	}
	post := Post{Shortcode: "X", Attachments: []Attachment{
		{Kind: "image", URL: ts.URL + "/img.jpg"},
		{Kind: "video", URL: ts.URL + "/small.mp4"},
		{Kind: "video", URL: ts.URL + "/big.mp4"},
	}}
	a.buildEmbedHTML(context.Background(), "https://oginstagram.com", "Discordbot", post, "p", 0, true, false)
	select {
	case path := <-requests:
		t.Fatalf("selected image triggered HEAD %s", path)
	default:
	}
	if html := a.buildEmbedHTML(context.Background(), "https://oginstagram.com", "Discordbot", post, "p", 1, true, false); !strings.Contains(html, `property="og:video"`) {
		t.Error("small selected video should expose og:video")
	}
	if path := <-requests; path != "/small.mp4" {
		t.Fatalf("probed %q, want selected small video", path)
	}
	a.buildEmbedHTML(context.Background(), "https://oginstagram.com", "Discordbot", post, "p", 1, true, false)
	select {
	case path := <-requests:
		t.Fatalf("cached selected video triggered another HEAD %s", path)
	default:
	}
	if html := a.buildEmbedHTML(context.Background(), "https://oginstagram.com", "Discordbot", post, "p", 2, true, false); strings.Contains(html, `property="og:video"`) {
		t.Error("oversized selected video should not expose og:video")
	}
	if path := <-requests; path != "/big.mp4" {
		t.Fatalf("probed %q, want selected big video", path)
	}
}

func TestHedgedPair(t *testing.T) {

	hedgeCalled := false
	p, err := hedgedPair(context.Background(),
		func(context.Context) (string, *AppError) { return "a", nil },
		func() attempt {
			hedgeCalled = true
			return func(context.Context) (string, *AppError) { return "", igErr(502, reasonGraphql, "x") }
		},
	)
	if err != nil || p != "a" {
		t.Fatalf("initial win: body=%q err=%+v", p, err)
	}
	if hedgeCalled {
		t.Error("hedge should not launch when the initial attempt answers first")
	}

	start := time.Now()
	p, err = hedgedPair(context.Background(),
		func(context.Context) (string, *AppError) { return "", igErr(502, reasonGraphql, "embed down") },
		func() attempt {
			return func(context.Context) (string, *AppError) { return "b", nil }
		},
	)
	if err != nil || p != "b" {
		t.Fatalf("hedge fallback: body=%q err=%+v", p, err)
	}
	if time.Since(start) > fetchHedgeDelay/2 {
		t.Error("hedge should launch on initial failure, not after the hedge delay")
	}

	_, err = hedgedPair(context.Background(),
		func(context.Context) (string, *AppError) { return "", igErr(502, reasonGraphql, "transient") },
		func() attempt {
			return func(context.Context) (string, *AppError) { return "", igErr(404, reasonMediaNotFound, "gone") }
		},
	)
	if err == nil || err.Reason != reasonMediaNotFound {
		t.Fatalf("want permanent error to win, got %+v", err)
	}

	_, err = hedgedPair(context.Background(),
		func(context.Context) (string, *AppError) { return "", igErr(502, reasonConnection, "down") },
		func() attempt { return nil },
	)
	if err == nil || err.Reason != reasonConnection {
		t.Fatalf("want primary error without hedge, got %+v", err)
	}
}

func TestHedgedPairCancelsLoser(t *testing.T) {
	loserStarted := make(chan struct{})
	loserCancelled := make(chan struct{})
	p, err := hedgedPair(context.Background(),
		func(context.Context) (string, *AppError) {
			<-loserStarted
			return "winner", nil
		},
		func() attempt {
			return func(ctx context.Context) (string, *AppError) {
				close(loserStarted)
				<-ctx.Done()
				close(loserCancelled)
				return "", igErr(499, "", "cancelled")
			}
		},
	)
	if err != nil || p != "winner" {
		t.Fatalf("winner: body=%q err=%+v", p, err)
	}
	select {
	case <-loserCancelled:
	case <-time.After(time.Second):
		t.Fatal("race loser was not cancelled")
	}
}

func TestConcurrentPostFallbacksPrefersThirdParty(t *testing.T) {
	backupStarted := make(chan struct{})
	post, ok := concurrentPostFallbacks(context.Background(),
		func(context.Context) (Post, bool) {
			<-backupStarted
			return Post{Shortcode: "preferred"}, true
		},
		func(context.Context) (Post, bool) {
			close(backupStarted)
			return Post{Shortcode: "backup"}, true
		},
	)
	if !ok || post.Shortcode != "preferred" {
		t.Fatalf("post=%+v ok=%v, want preferred", post, ok)
	}

	post, ok = concurrentPostFallbacks(context.Background(),
		func(context.Context) (Post, bool) { return Post{}, false },
		func(context.Context) (Post, bool) { return Post{Shortcode: "backup"}, true },
	)
	if !ok || post.Shortcode != "backup" {
		t.Fatalf("post=%+v ok=%v, want backup", post, ok)
	}
}

func TestDirectGetHonorsParentCancellation(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &App{direct: ts.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *AppError, 1)
	go func() {
		_, err := a.directGet(ctx, "post", "X", ts.URL)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Status != 499 {
			t.Fatalf("cancelled direct request error = %+v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct request ignored parent cancellation")
	}
}

func TestAttemptFetchTreatsCancelledBodyAsRaceLoser(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := &App{pool: &SessionPool{}}
	s := &Session{name: "test", client: ts.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *AppError, 1)
	go func() {
		_, err := a.attemptFetch(ctx, gqlSpec{name: "post", target: "X", method: http.MethodGet, url: ts.URL}, s)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Status != 499 {
			t.Fatalf("cancelled proxy body error = %+v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy body read ignored parent cancellation")
	}
}

func TestRaceFetchDoesNotReserveAfterCancellation(t *testing.T) {
	p := &SessionPool{sessions: []*Session{{}}}
	a := &App{pool: p}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.raceFetch(ctx, gqlSpec{})
	if err == nil || err.Status != 499 {
		t.Fatalf("cancelled race error = %+v", err)
	}
}

func TestParseOembedPost(t *testing.T) {
	body := `{
		"version": "1.0",
		"title": "time to create",
		"author_name": "instagram",
		"author_url": "https://www.instagram.com/instagram",
		"author_id": 25025320,
		"media_id": "3928250036051888465_25025320",
		"type": "rich",
		"html": "<a href=\"https://www.instagram.com/p/DaD8phTyclR/\" target=\"_blank\">A post shared by Instagram (@instagram)</a>",
		"thumbnail_url": "https://scontent-gmp1-1.cdninstagram.com/v/t51.82787-15/x.jpg?oe=6A4FBF9C",
		"thumbnail_width": 640,
		"thumbnail_height": 800
	}`
	p, ok := parseOembedPost("DaD8phTyclR", body)
	if !ok {
		t.Fatal("oembed success payload should parse")
	}
	if p.Username != "instagram" || p.FullName != "Instagram" || p.OwnerID != "25025320" {
		t.Errorf("author fields: %+v", p)
	}
	if p.Shortcode != "DaD8phTyclR" || !strings.Contains(p.Caption, "time to create") {
		t.Errorf("post fields: %+v", p)
	}
	att := p.Attachments[0]
	if att.ID != "3928250036051888465" || att.Kind != "image" || att.Width != 640 || att.Height != 800 {
		t.Errorf("attachment: %+v", att)
	}
	if att.URL == "" || att.URL != att.Thumbnail {
		t.Errorf("thumbnail-only attachment expected: %+v", att)
	}

	if _, ok := parseOembedPost("x", `{"status":"fail","title":"게시물을 사용할 수 없음","message":"삭제되었을 수 있습니다."}`); ok {
		t.Error("fail payload should not parse")
	}
}

func TestShortcodePK(t *testing.T) {
	if got := shortcodePK("DaEd82_pQ40"); got == nil || got.String() != "3928396500541181492" {
		t.Errorf("shortcodePK(DaEd82_pQ40) = %v, want 3928396500541181492", got)
	}
	if got := shortcodePK("has space"); got != nil {
		t.Errorf("invalid char should yield nil, got %v", got)
	}
}

func TestWebLoggedOutSpec(t *testing.T) {
	spec := webLoggedOutSpec("DaEd82_pQ40")
	if spec.method != http.MethodPost {
		t.Errorf("method = %q, want POST", spec.method)
	}
	if spec.url != "https://www.instagram.com/graphql/query" {
		t.Errorf("url = %q", spec.url)
	}
	if spec.headers["X-FB-Friendly-Name"] != "PolarisPostRootQuery" {
		t.Errorf("friendly name = %q", spec.headers["X-FB-Friendly-Name"])
	}

	if spec.headers["User-Agent"] != "Mozilla/5.0" {
		t.Errorf("user-agent = %q, want minimal Mozilla/5.0", spec.headers["User-Agent"])
	}
	vals, err := url.ParseQuery(spec.body)
	if err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if vals.Get("doc_id") != instagramWebLoggedOutDocID {
		t.Errorf("doc_id = %q, want %q", vals.Get("doc_id"), instagramWebLoggedOutDocID)
	}
	if lsd := vals.Get("lsd"); lsd == "" || lsd != spec.headers["X-FB-LSD"] {
		t.Errorf("lsd mismatch: body=%q header=%q", lsd, spec.headers["X-FB-LSD"])
	}
	v := vals.Get("variables")
	if !strings.Contains(v, `"shortcode":"DaEd82_pQ40"`) {
		t.Errorf("variables should hold shortcode, got %q", v)
	}
}
