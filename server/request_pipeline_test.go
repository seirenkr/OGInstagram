package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andybalholm/brotli"
)

type igAnswer struct {
	status int
	body   string
}

// fakeIG answers the post and profile sources by path and counts the calls. A
// zero answer means the source must not be asked.
type fakeIG struct {
	t                              *testing.T
	gql                            string
	gqlDelay                       time.Duration
	ruling, oembed, og, embed, api igAnswer
	calls                          sync.Map // source -> *atomic.Int32
}

func (f *fakeIG) count(source string) int32 {
	c, _ := f.calls.LoadOrStore(source, new(atomic.Int32))
	return c.(*atomic.Int32).Load()
}

func (f *fakeIG) pool() *SessionPool {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		source, answer := "", igAnswer{}
		switch {
		case strings.Contains(r.URL.Path, "get_ruling_for_content"):
			source, answer = "ruling", f.ruling
		case r.URL.Path == "/api/v1/oembed/":
			source, answer = "oembed", f.oembed
		case r.URL.Path == "/graphql/query":
			time.Sleep(f.gqlDelay)
			source, answer = "gql", igAnswer{200, f.gql}
		case r.URL.Path == "/api/graphql/":
			source, answer = "api", f.api
		case strings.Contains(r.URL.Path, "/embed/"):
			source, answer = "embed", f.embed
		case r.URL.Path == "/p/ABC123/":
			source, answer = "og", f.og
		}
		c, _ := f.calls.LoadOrStore(source, new(atomic.Int32))
		c.(*atomic.Int32).Add(1)
		if answer.status == 0 {
			f.t.Errorf("unexpected %s request %s", source, r.URL)
			answer.status = http.StatusInternalServerError
		}
		return mediaTestResponse(r, answer.status, nil, []byte(answer.body)), nil
	})}
	// Several sessions, so a rotated one does not hide the next request.
	return &SessionPool{sessions: []*Session{{name: "us-1", client: client}, {name: "us-2", client: client}, {name: "us-3", client: client}}}
}

const (
	gqlHidden     = `{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`
	gqlFull       = `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"ABC123","user":{"username":"instagram"},"display_url":"https://scontent.cdninstagram.com/image.jpg","media_type":1}]}}}`
	gateTitle12   = "People under 20 can't see this content"
	oembedGate3   = `{"status":"fail","message":"geoblock_required","gating_type_thrift":3,"title":"Gated","description":"d"}`
	oembedGate12  = `{"status":"fail","message":"geoblock_required","gating_type_thrift":12,"title":"` + gateTitle12 + `","description":"d"}`
	oembedPublic  = `{"author_name":"instagram","thumbnail_url":"https://scontent.cdninstagram.com/image.jpg","thumbnail_width":640,"thumbnail_height":640,"title":"cap"}`
	notFoundBody  = `{"message":"Media cannot be found","status":"fail"}`
	rulingOK      = `{"status":"ok"}`
	helperNoPost  = "external helper found no post"
	helperPostURL = "https://scontent.cdninstagram.com/x.jpg"
)

var (
	rulingGone = igAnswer{http.StatusNotFound, notFoundBody}
	oembedGone = igAnswer{http.StatusNotFound, notFoundBody}
)

func setHelper(t *testing.T, fn func(context.Context, string) (Post, *AppError)) *atomic.Int32 {
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	var calls atomic.Int32
	externalHelperPostImpl = func(_ *App, ctx context.Context, key string) (Post, *AppError) {
		calls.Add(1)
		return fn(ctx, key)
	}
	return &calls
}

func helperPost(key string) Post {
	return Post{Shortcode: key, Username: "instagram", Attachments: []Attachment{{Kind: "image", URL: helperPostURL}}}
}

// On 1675030 only the helper and oEmbed start at once; an account age gate
// needs no follow-up.
func TestFieldExceptionStartsHelperAndOembedAtOnce(t *testing.T) {
	helpers := setHelper(t, func(_ context.Context, key string) (Post, *AppError) {
		time.Sleep(300 * time.Millisecond)
		return helperPost(key), nil
	})
	synctest.Test(t, func(t *testing.T) {
		f := &fakeIG{t: t, gql: gqlHidden, ruling: igAnswer{200, rulingOK}, oembed: igAnswer{400, oembedGate12}}
		a := newApp(Config{}, f.pool(), offloadSigner{})
		started := time.Now()
		post, err := a.getPost(context.Background(), "ABC123", nil)
		if err != nil || post.Username != "instagram" || time.Since(started) != 300*time.Millisecond {
			t.Fatalf("getPost = %+v, %v after %s; want the helper's post without the 2 s hedge", post, err, time.Since(started))
		}
		if helpers.Load() != 1 || f.count("ruling") != 0 || f.count("oembed") != 1 || f.count("embed") != 0 || f.count("og") != 0 {
			t.Fatalf("helper=%d ruling=%d oEmbed=%d embed=%d", helpers.Load(), f.count("ruling"), f.count("oembed"), f.count("embed"))
		}
	})
}

func TestGoneNeedsRulingAndOembedToAgree(t *testing.T) {
	t.Run("both gone", func(t *testing.T) {
		helpers := setHelper(t, func(ctx context.Context, key string) (Post, *AppError) {
			select {
			case <-time.After(1500 * time.Millisecond):
				return helperPost(key), nil
			case <-ctx.Done():
				return Post{}, contextAppError(ctx)
			}
		})
		synctest.Test(t, func(t *testing.T) {
			f := &fakeIG{t: t, gql: gqlHidden, ruling: rulingGone, oembed: oembedGone}
			a := newApp(Config{}, f.pool(), offloadSigner{})
			started := time.Now()
			if _, err := a.getPost(context.Background(), "ABC123", nil); err == nil || err.Code != errorCodeMediaNotFound || time.Since(started) != 0 {
				t.Fatalf("getPost err = %v after %s; want media_not_found at once", err, time.Since(started))
			}
			time.Sleep(2 * time.Second)
			synctest.Wait()
			if entry, ok := a.posts.localGet("ABC123"); helpers.Load() != 1 || !ok || entry.err == nil {
				t.Fatal("the helper, cancelled for a gone post, replaced the verdict")
			}
		})
	})
	t.Run("ruling gone, oEmbed age gate", func(t *testing.T) {
		setHelper(t, func(context.Context, string) (Post, *AppError) {
			return Post{}, igErr(http.StatusNotFound, errorCodeUpstream, helperNoPost)
		})
		synctest.Test(t, func(t *testing.T) {
			f := &fakeIG{t: t, gql: gqlHidden, ruling: rulingGone, oembed: igAnswer{400, oembedGate12}}
			a := newApp(Config{}, f.pool(), offloadSigner{})
			if _, err := a.getPost(context.Background(), "ABC123", nil); err == nil || err.Code != errorCodeRestricted || err.CardTitle != gateTitle12 {
				t.Fatalf("getPost err = %#v; the ruling's 404 must not hide an age gate", err)
			}
		})
	})
}

func TestGateVerdictTTLDependsOnHelper(t *testing.T) {
	for _, tc := range []struct {
		name      string
		helperErr *AppError
		code      string
		ttl       time.Duration
	}{
		{"helper paused", ephemeralErr(http.StatusBadGateway, errorCodeCaptchaRequired, "external helper paused: verification failed"), errorCodeUpstream, transientErrorCacheSeconds * time.Second},
		{"helper found no post", igErr(http.StatusNotFound, errorCodeUpstream, helperNoPost), errorCodeRestricted, permanentErrorCacheSeconds * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHelper(t, func(context.Context, string) (Post, *AppError) { return Post{}, tc.helperErr })
			synctest.Test(t, func(t *testing.T) {
				f := &fakeIG{t: t, gql: gqlHidden, ruling: igAnswer{200, rulingOK}, oembed: igAnswer{400, oembedGate12}}
				a := newApp(Config{}, f.pool(), offloadSigner{})
				_, err := a.getPost(context.Background(), "ABC123", nil)
				if err == nil || err.Code != tc.code || err.CardCode != errorCodeRestricted || err.CardTitle != gateTitle12 {
					t.Fatalf("getPost error = %#v, want code %s with the gate card", err, tc.code)
				}
				if entry, ok := a.posts.localGet("ABC123"); !ok || time.Until(entry.expiresAt) != tc.ttl {
					t.Fatalf("cached verdict TTL wrong, want %s", tc.ttl)
				}
				if f.count("og") != 0 {
					t.Fatal("an account age gate has no Open Graph tags to fetch")
				}
			})
		})
	}
}

func TestPostLevelGateGetsOGCard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		err   *AppError
		ttl   time.Duration
	}{
		{"helper found no post", 3 * time.Second, igErr(http.StatusNotFound, errorCodeUpstream, helperNoPost), permanentErrorCacheSeconds * time.Second},
		{"helper rate limited", 0, igErr(http.StatusTooManyRequests, errorCodeRateLimited, "rate limited"), transientErrorCacheSeconds * time.Second},
		{"helper outlives the flight", cacheSharedWorkTimeout + 2*time.Second, igErr(http.StatusNotFound, errorCodeUpstream, helperNoPost), transientErrorCacheSeconds * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helpers := setHelper(t, func(context.Context, string) (Post, *AppError) {
				time.Sleep(tc.delay)
				return Post{}, tc.err
			})
			synctest.Test(t, func(t *testing.T) {
				f := &fakeIG{t: t, gql: gqlHidden, ruling: igAnswer{200, rulingOK}, oembed: igAnswer{400, oembedGate3}, og: igAnswer{200, ogTestPage("ABC123")}}
				store := newTestStore(t)
				a := newApp(Config{Store: store}, f.pool(), offloadSigner{})
				for n := range 2 {
					post, err := a.getPost(context.Background(), "ABC123", nil)
					if err != nil || !post.Partial || post.Username != "metaglasses" || post.Caption != "Nova. Sage." {
						t.Fatalf("getPost #%d = %+v, %v; want the partial post", n, post, err)
					}
				}
				if f.count("og") != 1 || helpers.Load() != 1 {
					t.Fatalf("og pages=%d helpers=%d; want one flight", f.count("og"), helpers.Load())
				}
				if entry, ok := a.posts.localGet("ABC123"); !ok || time.Until(entry.expiresAt) != tc.ttl {
					t.Fatalf("partial post cached=%v, want TTL %s", ok, tc.ttl)
				}
				synctest.Wait()
				var writes int
				if err := store.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&writes); err != nil || writes != 0 {
					t.Fatalf("persistent writes=%d, %v; partial posts stay out of SQLite", writes, err)
				}
				time.Sleep(max(tc.ttl, tc.delay))
			})
		})
	}
}

func TestPublicOembedCardWhenHelperFails(t *testing.T) {
	setHelper(t, func(context.Context, string) (Post, *AppError) {
		return Post{}, igErr(http.StatusNotFound, errorCodeUpstream, helperNoPost)
	})
	synctest.Test(t, func(t *testing.T) {
		f := &fakeIG{t: t, gql: gqlHidden, ruling: igAnswer{200, rulingOK}, oembed: igAnswer{200, oembedPublic}}
		a := newApp(Config{}, f.pool(), offloadSigner{})
		post, err := a.getPost(context.Background(), "ABC123", nil)
		if err != nil || post.Username != "instagram" || len(post.Attachments) != 1 || post.Attachments[0].URL != "https://scontent.cdninstagram.com/image.jpg" {
			t.Fatalf("getPost = %+v, %v; want oEmbed's thumbnail card", post, err)
		}
	})
}

func TestRenderDeadlineServesPartialOrGate(t *testing.T) {
	const helperDelay = 6 * time.Second
	for _, tc := range []struct {
		name, oembed, og string
		partial          bool
	}{
		{"post-level gate", oembedGate3, ogTestPage("ABC123"), true},
		{"account age gate", oembedGate12, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHelper(t, func(_ context.Context, key string) (Post, *AppError) {
				time.Sleep(helperDelay)
				return Post{Shortcode: key, Username: "metaglasses", Caption: "full",
					Attachments: []Attachment{{Kind: "video", URL: "https://scontent.cdninstagram.com/v.mp4", Thumbnail: "https://scontent.cdninstagram.com/t.jpg"}}}, nil
			})
			synctest.Test(t, func(t *testing.T) {
				f := &fakeIG{t: t, gql: gqlHidden, ruling: igAnswer{200, rulingOK}, oembed: igAnswer{400, tc.oembed}}
				if tc.og != "" {
					f.og = igAnswer{200, tc.og}
				}
				a := newApp(Config{}, f.pool(), mustOffloadSigner(testOffloadSigningKeys))
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
					if !strings.Contains(string(card.body), "People under 20") {
						t.Errorf("the deadline should show the gate notice: %d %s", card.originStatus, card.body)
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
				}
				time.Sleep(helperDelay)
				synctest.Wait()
				if res := serveApp(t, a, publicRequest("GET", "/p/ABC123/")); res.originStatus != 0 || !strings.Contains(string(res.body), "og:video") {
					t.Fatalf("the helper's full post did not replace the deadline answer: %d", res.originStatus)
				}
			})
		})
	}
}

func TestHelperCancelledWhenGraphQLWins(t *testing.T) {
	const graphQLDelay = externalHelperHedgeDelay + 100*time.Millisecond
	helperCancelled := make(chan struct{})
	setHelper(t, func(ctx context.Context, _ string) (Post, *AppError) {
		<-ctx.Done()
		close(helperCancelled)
		return Post{}, contextAppError(ctx)
	})
	synctest.Test(t, func(t *testing.T) {
		f := &fakeIG{t: t, gql: gqlFull, gqlDelay: graphQLDelay, embed: igAnswer{http.StatusInternalServerError, ""}}
		a := newApp(Config{}, f.pool(), offloadSigner{})
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

// Failures that say nothing about the exit IP, so no session rotates away
// from the fake transport between the two flights.
func TestFailedSourcesFallBackThenCache(t *testing.T) {
	helpers := setHelper(t, func(context.Context, string) (Post, *AppError) {
		return Post{}, igErr(http.StatusBadGateway, errorCodeUpstream, "external helper post unparseable")
	})
	synctest.Test(t, func(t *testing.T) {
		f := &fakeIG{t: t, gql: `{"data":{}}`, embed: igAnswer{200, "<html>not an embed</html>"}, oembed: igAnswer{200, `{"status":"fail"}`}}
		a := newApp(Config{}, f.pool(), offloadSigner{})
		for range 2 {
			if _, err := a.getPost(context.Background(), "ABC123", nil); err == nil || isTransient(err.Code) == false {
				t.Fatalf("final failure = %#v; want a transient error", err)
			}
		}
		if entry, ok := a.posts.localGet("ABC123"); !ok || time.Until(entry.expiresAt) != transientErrorCacheSeconds*time.Second {
			t.Fatal("transient failure should have the short error TTL")
		}
		if f.count("gql") != 1 || f.count("embed") != 1 || helpers.Load() != 1 || f.count("oembed") != 1 {
			t.Fatalf("GraphQL=%d embed=%d helper=%d oEmbed=%d; want one of each", f.count("gql"), f.count("embed"), helpers.Load(), f.count("oembed"))
		}
		time.Sleep(transientErrorCacheSeconds * time.Second)
		_, _ = a.getPost(context.Background(), "ABC123", nil)
		if f.count("gql") != 2 || helpers.Load() != 2 {
			t.Fatalf("transient failure did not retry after the short TTL: GraphQL=%d helper=%d embed=%d oEmbed=%d", f.count("gql"), helpers.Load(), f.count("embed"), f.count("oembed"))
		}
	})
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
			_, body, err := fetchRequest(context.Background(), client, fetchSpec{method: http.MethodGet, url: "https://example.com/", interpret: statusOnly, maxBytes: maxResponseBytes})
			if err != nil || len(body) != maxResponseBytes {
				t.Fatalf("cut body = %d bytes, %v", len(body), err)
			}
		})
	}
}

func TestProfileUsesRootContentAndPosts(t *testing.T) {
	const root = `{"data":{"xig_user_by_username":{"username":"instagram","biography":"bio","follower_count":5,"profile_pic_url":"https://scontent.cdninstagram.com/a.jpg"}}}`
	const posts = `{"data":{"xig_user_by_username":{"polaris_ordered_timeline_connection":{"edges":[{"node":{"display_uri":"https://scontent.cdninstagram.com/1.jpg"}}]}}}}`
	embed := wrapEmbed(`{"context":{"username":"instagram","posts_count":42,"followers_count":4,"profile_pic_url":"https://scontent.cdninstagram.com/e.jpg"}}`)
	for _, tc := range []struct {
		name       string
		root       string
		postsDelay time.Duration
		want       Profile
		thumbs     int
		embed      bool
	}{
		{"posts in time", root, 300 * time.Millisecond, Profile{Biography: "bio", FollowerCount: 5, MediaCount: unknownCount}, 1, false},
		{"posts too slow", root, 2 * time.Second, Profile{Biography: "bio", FollowerCount: 5, MediaCount: unknownCount}, 0, false},
		{"RootContent drifted", `{"data":{}}`, 0, Profile{FollowerCount: 4, MediaCount: 42}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var embeds atomic.Int32
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/graphql/" {
						if r.Header.Get("X-FB-Friendly-Name") == "PolarisLoggedOutDesktopWWWProfilePostsTabContentQuery" {
							time.Sleep(tc.postsDelay)
							return mediaTestResponse(r, 200, nil, []byte(posts)), nil
						}
						return mediaTestResponse(r, 200, nil, []byte(tc.root)), nil
					}
					embeds.Add(1)
					return mediaTestResponse(r, 200, nil, []byte(embed)), nil
				})}
				a := newApp(Config{}, &SessionPool{sessions: []*Session{{name: "us-1", client: client}}}, offloadSigner{})
				p, err := a.getProfile(context.Background(), "instagram", nil)
				if err != nil || p.Biography != tc.want.Biography || p.FollowerCount != tc.want.FollowerCount || p.MediaCount != tc.want.MediaCount {
					t.Fatalf("profile = %+v, %v; want %+v", p, err, tc.want)
				}
				if !tc.embed && len(p.RecentMedia) != tc.thumbs {
					t.Fatalf("thumbnails = %d, want %d", len(p.RecentMedia), tc.thumbs)
				}
				if (embeds.Load() > 0) != tc.embed {
					t.Fatalf("embed calls = %d; the embed only stands in for RootContent", embeds.Load())
				}
				time.Sleep(tc.postsDelay)
			})
		})
	}
}

// oEmbed's verdict picks the follow-up: GraphQL once more through DC for a
// regional gate or a public post, nothing for an account age gate.
func TestOembedVerdictPicksTheFollowUp(t *testing.T) {
	for _, tc := range []struct {
		name, oembed string
		status       int
		og           bool
		dcRetry      bool
	}{
		{"regional gate", `{"status":"fail","message":"geoblock_required","gating_type_thrift":2,"title":"Restricted Post","description":"d"}`, 400, true, true},
		{"public", oembedPublic, 200, false, true},
		{"account age gate", oembedGate12, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHelper(t, func(context.Context, string) (Post, *AppError) {
				return Post{}, igErr(http.StatusNotFound, errorCodeUpstream, helperNoPost)
			})
			synctest.Test(t, func(t *testing.T) {
				// IPv6 hides the post; the DC proxy's GraphQL shows it.
				f := &fakeIG{t: t, gql: gqlFull, oembed: igAnswer{tc.status, tc.oembed}}
				if tc.og {
					f.og = igAnswer{200, ogTestPage("ABC123")}
				}
				var v6GQL atomic.Int32
				v6 := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/graphql/query" {
						v6GQL.Add(1)
						return mediaTestResponse(r, 200, nil, []byte(gqlHidden)), nil
					}
					return f.pool().sessions[0].client.Transport.RoundTrip(r)
				})}
				v6Pool := func() *SessionPool {
					return &SessionPool{proxyURL: func() *url.URL { return &url.URL{Scheme: "http", User: url.User("x"), Host: "egress"} }, sessions: []*Session{{name: "v6-1", client: v6}, {name: "v6-2", client: v6}}}
				}
				a := newApp(Config{EgressPercent: 100}, f.pool(), offloadSigner{})
				a.v6, a.v6Pages = v6Pool(), v6Pool()
				post, err := a.getPost(context.Background(), "ABC123", nil)
				if tc.dcRetry && (err != nil || post.Partial || post.Username != "instagram") {
					t.Fatalf("getPost = %+v, %v; want the DC GraphQL post", post, err)
				}
				if got := f.count("gql"); (got == 1) != tc.dcRetry || v6GQL.Load() != 1 {
					t.Fatalf("DC GraphQL calls = %d, IPv6 = %d; want DC retry %t", got, v6GQL.Load(), tc.dcRetry)
				}
				if f.count("ruling") != 0 {
					t.Fatal("the ruling only follows an oEmbed 404")
				}
			})
		})
	}
}
