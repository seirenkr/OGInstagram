package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andybalholm/brotli"
)

// stubPausedEmbed fails the test on any direct request but the post page,
// which answers with page.
func stubPausedEmbed(t *testing.T, page string) {
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/p/ABC123/" {
			return mediaTestResponse(r, http.StatusOK, nil, []byte(page)), nil
		}
		t.Error("paused embed unexpectedly fetched")
		return mediaTestResponse(r, http.StatusInternalServerError, nil, nil), nil
	}))
}

func TestPausedEmbedFailureIsCached(t *testing.T) {
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	var helpers atomic.Int32
	externalHelperPostImpl = func(_ *App, _ context.Context, _ string) (Post, *AppError) {
		helpers.Add(1)
		return Post{}, igErr(http.StatusBadGateway, errorCodeUpstream, "external helper post unparseable")
	}
	stubPausedEmbed(t, ogAccountGatedPage)
	synctest.Test(t, func(t *testing.T) {
		var gql, oembed atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "get_ruling_for_content") {
				return mediaTestResponse(r, 200, nil, []byte(`{"status":"ok"}`)), nil
			}
			if r.URL.Path == "/api/v1/oembed/" {
				oembed.Add(1)
				return mediaTestResponse(r, 200, nil, []byte(`{"status":"fail"}`)), nil
			}
			gql.Add(1)
			return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(2 * time.Minute).UnixNano())
		for n := 0; n < 2; n++ {
			_, err := a.getPost(context.Background(), "ABC123", nil)
			if err == nil || err.Ephemeral || err.Code != errorCodeGraphQL || err.Status == 404 {
				t.Fatalf("final failure = %#v; want cacheable transient GraphQL error", err)
			}
		}
		entry, ok := a.posts.localGet("ABC123")
		if !ok || time.Until(entry.expiresAt) != transientErrorCacheSeconds*time.Second {
			t.Fatal("transient failure should have the short error TTL")
		}
		if gql.Load() != 1 || oembed.Load() != 1 || helpers.Load() != 1 {
			t.Fatalf("duplicate calls: GraphQL=%d oEmbed=%d helper=%d", gql.Load(), oembed.Load(), helpers.Load())
		}
		time.Sleep(transientErrorCacheSeconds * time.Second)
		_, _ = a.getPost(context.Background(), "ABC123", nil)
		if gql.Load() != 2 || oembed.Load() != 2 || helpers.Load() != 2 {
			t.Fatal("transient failure did not retry after the short TTL")
		}
	})
}

func TestOembedNotFoundIsPermanentOnlyAfterHelperAnswered(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ruling    int
		helperErr *AppError
		want      string
	}{
		{"helper answered", http.StatusUnauthorized, igErr(http.StatusNotFound, errorCodeUpstream, "external helper found no post"), errorCodeMediaNotFound},
		{"ruling says it exists", http.StatusOK, igErr(http.StatusNotFound, errorCodeUpstream, "external helper found no post"), errorCodeGraphQL},
		{"helper captcha", http.StatusUnauthorized, ephemeralErr(http.StatusBadGateway, errorCodeCaptchaRequired, "external helper paused: captcha"), errorCodeGraphQL},
		{"helper outage", http.StatusUnauthorized, igErr(http.StatusServiceUnavailable, errorCodeUpstream, "Service Unavailable"), errorCodeGraphQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(*App, context.Context, string) (Post, *AppError) { return Post{}, tc.helperErr }
			stubPausedEmbed(t, ogAccountGatedPage)
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "get_ruling_for_content") {
						return mediaTestResponse(r, tc.ruling, nil, []byte(`{"status":"ok"}`)), nil
					}
					if r.URL.Path == "/api/v1/oembed/" {
						return mediaTestResponse(r, 404, nil, []byte(`{"message":"Media cannot be found","status":"fail"}`)), nil
					}
					return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
				})}
				pool := &SessionPool{sessions: []*Session{{client: client}}}
				a := newApp(Config{}, pool, offloadSigner{})
				a.embedPausedUntil.Store(time.Now().Add(time.Minute).UnixNano())
				if _, err := a.getPost(context.Background(), "ABC123", nil); err == nil || err.Code != tc.want {
					t.Fatalf("getPost error = %#v, want %s", err, tc.want)
				}
			})
		})
	}
}

func TestGatedVerdictIsShortWhileHelperCouldNotAsk(t *testing.T) {
	for _, tc := range []struct {
		name      string
		helperErr *AppError
		code      string
		ttl       time.Duration
	}{
		{"helper paused", ephemeralErr(http.StatusBadGateway, errorCodeCaptchaRequired, "external helper paused: verification failed"), errorCodeUpstream, transientErrorCacheSeconds * time.Second},
		{"helper found no post", igErr(http.StatusNotFound, errorCodeUpstream, "external helper found no post"), errorCodeRestricted, permanentErrorCacheSeconds * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(*App, context.Context, string) (Post, *AppError) { return Post{}, tc.helperErr }
			stubPausedEmbed(t, ogAccountGatedPage)
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "get_ruling_for_content") {
						return mediaTestResponse(r, 200, nil, []byte(`{"status":"ok"}`)), nil
					}
					if r.URL.Path == "/api/v1/oembed/" {
						return mediaTestResponse(r, 400, nil, []byte(`{"status":"fail","message":"geoblock_required","gating_type_thrift":3,"title":"Gated","description":"d"}`)), nil
					}
					return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
				})}
				pool := &SessionPool{sessions: []*Session{{client: client}}}
				a := newApp(Config{}, pool, offloadSigner{})
				a.embedPausedUntil.Store(time.Now().Add(time.Minute).UnixNano())
				_, err := a.getPost(context.Background(), "ABC123", nil)
				if err == nil || err.Code != tc.code || err.CardCode != errorCodeRestricted || err.CardTitle != "Gated" {
					t.Fatalf("getPost error = %#v, want code %s with the gated card", err, tc.code)
				}
				if entry, ok := a.posts.localGet("ABC123"); !ok || time.Until(entry.expiresAt) != tc.ttl {
					t.Fatalf("cached verdict TTL wrong, want %s", tc.ttl)
				}
			})
		})
	}
}

func TestLateFinalVerdictKeepsHelperAndFillsCache(t *testing.T) {
	const graphQLDelay = externalHelperHedgeDelay + 100*time.Millisecond
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	var helpers atomic.Int32
	externalHelperPostImpl = func(_ *App, ctx context.Context, key string) (Post, *AppError) {
		helpers.Add(1)
		select {
		case <-time.After(1500 * time.Millisecond):
			return Post{Shortcode: key, Username: "instagram", Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/x.jpg"}}}, nil
		case <-ctx.Done():
			return Post{}, contextAppError(ctx)
		}
	}
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("paused embed unexpectedly fetched")
		return mediaTestResponse(r, http.StatusInternalServerError, nil, nil), nil
	}))
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "get_ruling_for_content") {
				return mediaTestResponse(r, 404, nil, []byte(`{"status":"fail","message":"Media cannot be found"}`)), nil
			}
			time.Sleep(graphQLDelay)
			return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(time.Minute).UnixNano())
		started := time.Now()
		_, err := a.getPost(context.Background(), "ABC123", nil)
		if err == nil || !err.Final || time.Since(started) != graphQLDelay {
			t.Fatal("missing verdict should return without waiting for helper")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		entry, ok := a.posts.localGet("ABC123")
		if helpers.Load() != 1 || !ok || entry.err != nil || entry.value.Username != "instagram" {
			t.Fatal("existing helper should continue once and replace the missing verdict")
		}
	})
}

func TestHelperCancelledWhenGraphQLWins(t *testing.T) {
	const graphQLDelay = externalHelperHedgeDelay + 100*time.Millisecond
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	helperCancelled := make(chan struct{})
	externalHelperPostImpl = func(_ *App, ctx context.Context, _ string) (Post, *AppError) {
		<-ctx.Done()
		close(helperCancelled)
		return Post{}, contextAppError(ctx)
	}
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("paused embed unexpectedly fetched")
		return mediaTestResponse(r, http.StatusInternalServerError, nil, nil), nil
	}))
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			time.Sleep(graphQLDelay)
			return mediaTestResponse(r, 200, nil, []byte(`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"ABC123","user":{"username":"instagram"},"display_url":"https://scontent.cdninstagram.com/image.jpg","media_type":1}]}}}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(time.Minute).UnixNano())
		if _, err := a.getPost(context.Background(), "ABC123", nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-helperCancelled:
		default:
			t.Fatal("losing helper kept consuming resources after a full result")
		}
	})
}

// gatedPostProxy answers like Instagram for a post-level gate: GraphQL fails
// with field_exception, the ruling says the post exists, oEmbed reports the gate.
func gatedPostProxy(oembed *atomic.Int32) *SessionPool {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "get_ruling_for_content") {
			return mediaTestResponse(r, 200, nil, []byte(`{"status":"ok"}`)), nil
		}
		if r.URL.Path == "/api/v1/oembed/" {
			oembed.Add(1)
			return mediaTestResponse(r, 400, nil, []byte(`{"status":"fail","message":"geoblock_required","gating_type_thrift":3,"title":"Gated","description":"d"}`)), nil
		}
		return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
	})}
	return &SessionPool{sessions: []*Session{{client: client}}}
}

func TestOGCardIsTheResultWhenHelperFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		err   *AppError
		ttl   time.Duration // 0: handed to the flight's waiters, never stored
	}{
		{"helper found no post", 3 * time.Second, igErr(http.StatusNotFound, errorCodeUpstream, "external helper found no post"), permanentErrorCacheSeconds * time.Second},
		{"helper rate limited", 0, igErr(http.StatusTooManyRequests, errorCodeRateLimited, "rate limited"), transientErrorCacheSeconds * time.Second},
		{"helper outlives the flight", cacheSharedWorkTimeout + 2*time.Second, igErr(http.StatusNotFound, errorCodeUpstream, "external helper found no post"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			var helpers, ogPages atomic.Int32
			externalHelperPostImpl = func(*App, context.Context, string) (Post, *AppError) {
				helpers.Add(1)
				time.Sleep(tc.delay)
				return Post{}, tc.err
			}
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/p/ABC123/" || r.Header.Get("User-Agent") != embedUA {
					t.Errorf("unexpected direct request %s", r.URL)
				}
				ogPages.Add(1)
				return mediaTestResponse(r, http.StatusOK, nil, []byte(ogTestPage("ABC123"))), nil
			}))
			synctest.Test(t, func(t *testing.T) {
				var oembed atomic.Int32
				store := newTestStore(t)
				a := newApp(Config{Store: store}, gatedPostProxy(&oembed), offloadSigner{})
				a.embedPausedUntil.Store(time.Now().Add(24 * time.Hour).UnixNano())
				for n := range 2 {
					post, err := a.getPost(context.Background(), "ABC123", nil)
					if err != nil || !post.Partial || post.Username != "metaglasses" || post.Caption != "Nova. Sage." {
						t.Fatalf("getPost #%d = %+v, %v; want the partial post", n, post, err)
					}
				}
				flights := int32(1)
				if tc.ttl == 0 {
					flights = 2
				}
				if ogPages.Load() != flights || helpers.Load() != flights || oembed.Load() != 0 {
					t.Fatalf("og pages=%d helpers=%d oEmbed=%d; want %d flights without a gate lookup", ogPages.Load(), helpers.Load(), oembed.Load(), flights)
				}
				entry, ok := a.posts.localGet("ABC123")
				if tc.ttl == 0 && ok || tc.ttl > 0 && (!ok || time.Until(entry.expiresAt) != tc.ttl) {
					t.Fatalf("partial post cached=%v, want TTL %s", ok, tc.ttl)
				}
				synctest.Wait()
				var writes int
				if err := store.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&writes); err != nil || writes != 0 {
					t.Fatalf("persistent writes=%d, %v; partial posts stay out of SQLite", writes, err)
				}
				if tc.ttl == 0 {
					time.Sleep(tc.delay) // the bubble outlives the abandoned helpers
					return
				}
				time.Sleep(tc.ttl)
				if _, err := a.getPost(context.Background(), "ABC123", nil); err != nil || helpers.Load() != 2 || ogPages.Load() != 2 {
					t.Fatalf("expired partial post did not retry the helper: %v", err)
				}
			})
		})
	}
}

func TestOGPageOnlyForFieldException(t *testing.T) {
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	externalHelperPostImpl = func(*App, context.Context, string) (Post, *AppError) {
		return Post{}, igErr(http.StatusNotFound, errorCodeUpstream, "external helper found no post")
	}
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected direct request %s", r.URL)
		return mediaTestResponse(r, http.StatusInternalServerError, nil, nil), nil
	}))
	for _, body := range []string{`{"data":null,"errors":[{"message":"x","code":1357001}]}`, `{"data":{"renamed":{}}}`} {
		synctest.Test(t, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "get_ruling_for_content") {
					return mediaTestResponse(r, 200, nil, []byte(`{"status":"ok"}`)), nil
				}
				if r.URL.Path == "/api/v1/oembed/" {
					return mediaTestResponse(r, 200, nil, []byte(`{"status":"fail"}`)), nil
				}
				return mediaTestResponse(r, 200, nil, []byte(body)), nil
			})}
			a := newApp(Config{}, &SessionPool{sessions: []*Session{{client: client}}}, offloadSigner{})
			a.embedPausedUntil.Store(time.Now().Add(time.Hour).UnixNano())
			if _, err := a.getPost(context.Background(), "ABC123", nil); err == nil || err.Code != errorCodeGraphQL {
				t.Fatalf("getPost error = %#v for %s", err, body)
			}
		})
	}
}

func TestOGPagePausesAfterBlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls, status := 0, http.StatusTooManyRequests
		stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return mediaTestResponse(r, status, nil, []byte(ogAccountGatedPage)), nil
		}))
		a := &App{}
		if _, err := a.fetchPostOG(context.Background(), "ABC123"); err == nil || calls != 1 {
			t.Fatal("blocked page made a card")
		}
		if got := time.Until(time.Unix(0, a.ogPausedUntil.Load())); got != ogPageProbeInterval {
			t.Fatalf("pause = %s, want %s", got, ogPageProbeInterval)
		}
		if _, err := a.fetchPostOG(context.Background(), "ABC123"); err == nil || calls != 1 {
			t.Fatal("paused page was fetched")
		}
		time.Sleep(ogPageProbeInterval)
		status = http.StatusOK
		// A page without tags is still an answer, so the probe reopens the path.
		if _, err := a.fetchPostOG(context.Background(), "ABC123"); err == nil || calls != 2 || a.ogPausedUntil.Load() != 0 {
			t.Fatalf("probe: calls=%d paused=%d", calls, a.ogPausedUntil.Load())
		}
	})
}

func TestRenderDeadlineServesOGCard(t *testing.T) {
	const helperDelay = 6 * time.Second
	for _, tc := range []struct {
		name, page string
		partial    bool
	}{
		{"og tags", ogTestPage("ABC123"), true},
		{"no og tags", ogAccountGatedPage, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(_ *App, _ context.Context, key string) (Post, *AppError) {
				time.Sleep(helperDelay)
				return Post{Shortcode: key, Username: "metaglasses", Caption: "full",
					Attachments: []Attachment{{Kind: "video", URL: "https://scontent.cdninstagram.com/v.mp4", Thumbnail: "https://scontent.cdninstagram.com/t.jpg"}}}, nil
			}
			stubPausedEmbed(t, tc.page)
			synctest.Test(t, func(t *testing.T) {
				var oembed atomic.Int32
				a := newApp(Config{}, gatedPostProxy(&oembed), mustOffloadSigner(testOffloadSigningKeys))
				a.embedPausedUntil.Store(time.Now().Add(time.Hour).UnixNano())
				cover := normalizeCDNHost(ogTestCover)
				requests := []*http.Request{publicRequest("GET", "/p/ABC123/"), publicRequest("GET", "/offload/ABC123/1"), publicRequest("GET", "/p/ABC123")}
				requests[2].Host = "d.oginstagram.com"
				results := make([]resp, len(requests))
				var wg sync.WaitGroup
				for i, r := range requests {
					wg.Go(func() {
						ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
						defer cancel()
						results[i] = serveApp(t, a, r.WithContext(ctx))
					})
				}
				wg.Wait()
				card, offload, direct := results[0], results[1], results[2]
				if !tc.partial {
					if card.originStatus != http.StatusGatewayTimeout || !strings.Contains(string(card.body), "Temporarily unavailable") ||
						offload.headers["Location"] != instagramOrigin+"/p/ABC123/" {
						t.Errorf("without tags the deadline must keep today's answer: %d %v", card.originStatus, offload.headers)
					}
				} else {
					if card.status != 200 || card.originStatus != http.StatusNonAuthoritativeInfo || !strings.Contains(string(card.body), "Nova. Sage.") {
						t.Errorf("card at the deadline = %d/%d %.300s", card.status, card.originStatus, card.body)
					}
					for _, res := range []resp{offload, direct} {
						if res.status != 302 || res.headers["Location"] != cover || res.originStatus != http.StatusNonAuthoritativeInfo {
							t.Errorf("media at the deadline = %d/%d %v, want the cover", res.status, res.originStatus, res.headers)
						}
					}
					// The card's media come at once while the flight runs on, and
					// only item 1 is the cover.
					started := time.Now()
					for target, want := range map[string]string{"/offload/ABC123/1?thumbnail=1": cover, "/offload/ABC123/3": instagramOrigin + "/p/ABC123/"} {
						if res := serveApp(t, a, publicRequest("GET", target)); res.status != 302 || res.headers["Location"] != want ||
							res.originStatus != http.StatusNonAuthoritativeInfo || time.Since(started) != 0 {
							t.Errorf("%s after %s = %d/%d %v, want %s", target, time.Since(started), res.status, res.originStatus, res.headers, want)
						}
					}
				}
				time.Sleep(helperDelay)
				synctest.Wait()
				if res := serveApp(t, a, publicRequest("GET", "/p/ABC123/")); res.originStatus != 0 || !strings.Contains(string(res.body), "og:video") {
					t.Fatalf("the helper's full post did not replace the partial card: %d", res.originStatus)
				}
				if oembed.Load() != 0 {
					t.Fatal("oEmbed was asked although the helper answered")
				}
			})
		})
	}
}

type unreadResponseBody struct {
	reads  int
	closed bool
}

func (b *unreadResponseBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadResponseBody) Close() error             { b.closed = true; return nil }

func TestJSONRejectsHTMLAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		kind   string
	}{
		{"html", 200, "text/html; charset=utf-8"},
		{"xhtml", 200, "application/xhtml+xml"},
		{"forbidden_html", 403, "text/html"},
		{"login_redirect", 302, "text/html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, redirects := 0, 0
			body := &unreadResponseBody{}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.kind}, "Location": []string{instagramOrigin + "/accounts/login/"}}, Body: body, Request: r}, nil
			})}
			status, _, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
			if status != tc.status || err == nil || calls != 1 || redirects != 0 || (tc.status == 302) != (body.reads == 0) || !body.closed {
				t.Fatalf("status=%d err=%v calls=%d redirects=%d reads=%d closed=%t", status, err, calls, redirects, body.reads, body.closed)
			}
			if err := client.CheckRedirect(nil, nil); err != nil || redirects != 1 {
				t.Fatal("shared client's redirect policy was changed")
			}
		})
	}
}

func TestHTMLSourceStillFollowsRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return mediaTestResponse(r, 302, http.Header{"Location": []string{"https://example.com/page"}}, nil), nil
		}
		return mediaTestResponse(r, 200, http.Header{"Content-Type": []string{"text/html"}}, []byte("<html>page</html>")), nil
	})}
	_, body, err := fetchRequest(context.Background(), client, fetchSpec{method: http.MethodGet, url: "https://example.com/start", interpret: statusOnly})
	if err != nil || body != "<html>page</html>" || calls != 2 {
		t.Fatal("HTML source redirect/response behavior changed")
	}
}

func compressedResponse(t *testing.T, encoding string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	default:
		return raw
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchRequestCompression(t *testing.T) {
	const raw = `{"data":{"ok":true}}`
	for _, encoding := range []string{"", "identity", "gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			wire := compressedResponse(t, encoding, []byte(raw))
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Accept-Encoding") != "br, gzip" {
					t.Error("missing compression negotiation")
				}
				return mediaTestResponse(r, 200, http.Header{"Content-Encoding": []string{encoding}, "Content-Type": []string{"text/javascript; charset=utf-8"}}, wire), nil
			})}
			_, body, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
			if err != nil || body != raw {
				t.Fatalf("decoded response = %q, %v", body, err)
			}
		})
	}
	t.Run("already_decompressed", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp := mediaTestResponse(r, 200, nil, []byte(raw))
			resp.Uncompressed = true
			return resp, nil
		})}
		_, body, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
		if err != nil || body != raw {
			t.Fatal("already-decompressed response was decoded again")
		}
	})
}

func TestFetchRequestCompressedBodyLimits(t *testing.T) {
	for _, encoding := range []string{"gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			wire := compressedResponse(t, encoding, []byte(strings.Repeat("x", maxResponseBytes+1)))
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaTestResponse(r, 200, http.Header{"Content-Encoding": []string{encoding}}, wire), nil
			})}
			_, _, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
			if err == nil || !strings.Contains(err.PublicMessage, "too large") {
				t.Fatal("decoded body limit was not enforced")
			}
			_, body, err := fetchRequest(context.Background(), client, fetchSpec{method: http.MethodGet, url: "https://example.com/", interpret: statusOnly, cut: true})
			if err != nil || len(body) != maxResponseBytes {
				t.Fatalf("cut body = %d bytes, %v", len(body), err)
			}
		})
	}
}
