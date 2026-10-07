package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCachePolicyUsesSuccessfulSource(t *testing.T) {
	const shortcode = "DaD8phTyclR"
	for _, tc := range []struct {
		name, kind, source string
		persist            bool
	}{
		{"captioned_after_proxy_started", "post", "captioned", false},
		{"post_graphql", "post", "graphql", true},
		{"post_external_helper", "post", "helper", true},
		{"post_oembed", "post", "oembed", true},
		{"profile_api", "profile", "api", true},
		{"profile_embed", "profile", "embed", false},
		{"story_external_helper", "story", "helper", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				response := func(status int, body string) *http.Response {
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
				}
				var originCalls atomic.Int32
				proxyStarted, proxyDone := make(chan struct{}), make(chan struct{})
				stubDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
					originCalls.Add(1)
					switch tc.source {
					case "captioned":
						select {
						case <-proxyStarted:
							return response(200, simpleEmbedImagePage), nil
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					case "embed":
						return response(200, `{"contextJSON":"{\"context\":{\"username\":\"instagram\",\"profile_pic_url\":\"https://cdn.example/avatar.jpg\"}}"}`), nil
					default:
						return response(200, "invalid embed"), nil
					}
				}))
				proxyClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					originCalls.Add(1)
					switch tc.source {
					case "captioned":
						close(proxyStarted)
						defer close(proxyDone)
						<-req.Context().Done()
						return nil, req.Context().Err()
					case "graphql":
						return response(200, `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"DaD8phTyclR","user":{"username":"instagram"},"display_url":"https://cdn.example/image.jpg","media_type":1}]}}}`), nil
					case "api":
						return response(200, `{"data":{"user":{"username":"instagram","profile_pic_url":"https://cdn.example/avatar.jpg"}}}`), nil
					case "oembed":
						if req.URL.Path == "/api/v1/oembed/" {
							return response(200, `{"author_name":"instagram","thumbnail_url":"https://cdn.example/image.jpg","title":"oEmbed"}`), nil
						}
					}
					return response(200, `{"data":{}}`), nil
				})}
				pool := &SessionPool{
					sessions:             []*Session{{client: proxyClient}},
					budgetLeaseExpires:   time.Now().Add(time.Hour),
					budgetLeaseRemaining: 1 << 20,
				}
				previousPost, previousStory := externalHelperPostImpl, externalHelperStoryImpl
				t.Cleanup(func() {
					externalHelperPostImpl, externalHelperStoryImpl = previousPost, previousStory
				})
				externalHelperPostImpl = func(_ *App, _ context.Context, key string) (Post, bool) {
					originCalls.Add(1)
					return Post{Shortcode: key, Username: "instagram", Attachments: []Attachment{{Kind: "image", URL: "https://cdn.example/image.jpg"}}}, tc.source == "helper"
				}
				externalHelperStoryImpl = func(_ *App, _ context.Context, username, id string) (Story, *AppError) {
					originCalls.Add(1)
					return Story{ID: id, Username: username, Media: Attachment{Kind: "image", URL: "https://cdn.example/story.jpg"}}, nil
				}
				store := newTestStore(t)
				a := newApp(Config{Store: store}, pool, offloadSigner{})
				get := func(meta *fetchMeta) *AppError {
					switch tc.kind {
					case "post":
						_, err := a.getPost(context.Background(), shortcode, meta)
						return err
					case "profile":
						_, err := a.getProfile(context.Background(), "instagram", meta)
						return err
					default:
						_, err := a.getStory(context.Background(), "instagram", "123", meta)
						return err
					}
				}
				first := &fetchMeta{}
				if err := get(first); err != nil || !first.fetched {
					t.Fatalf("first get = %#v, fetched=%t; want successful origin result", err, first.fetched)
				}
				if tc.source == "captioned" {
					select {
					case <-proxyDone:
					case <-time.After(time.Second):
						t.Fatal("losing proxy was not cancelled")
					}
				}
				calls := originCalls.Load()
				second := &fetchMeta{}
				if err := get(second); err != nil || second.fetched || originCalls.Load() != calls {
					t.Fatalf("second get = %#v, fetched=%t, origin calls=%d→%d; want L1 hit", err, second.fetched, calls, originCalls.Load())
				}
				var writes int
				if err := store.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&writes); err != nil {
					t.Fatal(err)
				}
				want := 0
				if tc.persist {
					want = 1
				}
				if writes != want {
					t.Fatalf("persistent model writes=%d, want %d for successful source", writes, want)
				}
			})
		})
	}
}

func TestFetchPostFallbacks(t *testing.T) {
	// The embed parses, but its plain-HTTP image would be refused by the cache.
	invalidEmbed := wrapEmbed(`{"context":{"shortcode":"ABC123"},"gql_data":{"shortcode_media":{"__typename":"GraphImage","id":"1","shortcode":"ABC123",` +
		`"is_video":false,"owner":{"id":"9","username":"alice"},"display_url":"http://insecure.example/x.jpg","dimensions":{"width":1,"height":1}}}}`)
	graphql := `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"ABC123","user":{"username":"bob"},` +
		`"display_url":"https://scontent.cdninstagram.com/x.jpg","media_type":1}]}}}`
	for _, tc := range []struct {
		name, graphql, wantUser string
		degraded                bool
	}{
		{"invalid embed loses to GraphQL", graphql, "bob", false},
		{"ready oEmbed card beats hung sources", "", "carol", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Swap globals outside the bubble: synctest.Test returns only after the
			// cancelled losers exit, so the restore cannot race them.
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(_ *App, ctx context.Context, _ string) (Post, bool) {
				<-ctx.Done()
				return Post{}, false
			}
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaTestResponse(r, 200, nil, []byte(invalidEmbed)), nil
			}))
			synctest.Test(t, func(t *testing.T) {
				proxy := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/oembed/" {
						return mediaTestResponse(r, 200, nil, []byte(`{"author_name":"carol","thumbnail_url":"https://scontent.cdninstagram.com/t.jpg","title":"t"}`)), nil
					}
					if tc.graphql == "" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return mediaTestResponse(r, 200, nil, []byte(tc.graphql)), nil
				})}
				pool := &SessionPool{sessions: []*Session{{client: proxy}},
					budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}
				a := newApp(Config{}, pool, offloadSigner{})
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				defer cancel()
				started := time.Now()
				post, err := a.getPost(ctx, "ABC123", nil)
				if err != nil || post.Username != tc.wantUser || time.Since(started) >= requestTimeout {
					t.Fatalf("getPost = %q, %v after %s", post.Username, err, time.Since(started))
				}
				entry, ok := a.posts.localGet("ABC123")
				if short := ok && time.Until(entry.expiresAt) <= transientErrorCacheSeconds*time.Second; short != tc.degraded {
					t.Fatalf("degraded=%v but cached until %v", tc.degraded, entry.expiresAt)
				}
			})
		})
	}
}

func TestEmbedPausesAfterRateLimit(t *testing.T) {
	var hits, status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return mediaTestResponse(r, int(status.Load()), nil, []byte(simpleEmbedImagePage)), nil
	}))
	synctest.Test(t, func(t *testing.T) {
		a := &App{}
		ctx := context.Background()
		if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err == nil || err.Status != http.StatusTooManyRequests {
			t.Fatalf("first call = %v, want 429", err)
		}
		if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err == nil || hits.Load() != 1 {
			t.Fatalf("paused embed still reached Instagram (%d hits, err %v)", hits.Load(), err)
		}
		time.Sleep(embedProbeInterval)
		status.Store(http.StatusOK)
		if _, err := a.fetchPostEmbed(ctx, "DaD8phTyclR"); err != nil || hits.Load() != 2 {
			t.Fatalf("probe after the interval = %v with %d hits, want one successful probe", err, hits.Load())
		}
		if a.embedPausedUntil.Load() != 0 {
			t.Fatal("a successful probe must clear the pause")
		}
	})
}

func TestRulingMissAnswersAtOnceAndHelperFillsCache(t *testing.T) {
	const shortcode = "ABC123"
	gqlError := `{"data":null,"errors":[{"message":"A server error field_exception occured.","code":1675030}]}`
	helperPost := Post{Shortcode: shortcode, Username: "dave",
		Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/x.jpg"}}}
	for _, tc := range []struct {
		name, ruling string
		rulingStatus int
		missing      bool
	}{
		{"ruling miss answers 404 at once", `{"message":"Media cannot be found","status":"fail"}`, http.StatusNotFound, true},
		{"gated post still waits for the helper", `{"title":"This content isn't available to everyone","status":"ok"}`, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(_ *App, ctx context.Context, _ string) (Post, bool) {
				select {
				case <-time.After(1500 * time.Millisecond):
					return helperPost, true
				case <-ctx.Done():
					return Post{}, false
				}
			}
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "get_ruling_for_content") {
					return mediaTestResponse(r, tc.rulingStatus, nil, []byte(tc.ruling)), nil
				}
				return mediaTestResponse(r, http.StatusTooManyRequests, nil, nil), nil
			}))
			synctest.Test(t, func(t *testing.T) {
				proxy := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/oembed/" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return mediaTestResponse(r, 200, nil, []byte(gqlError)), nil
				})}
				pool := &SessionPool{sessions: []*Session{{client: proxy}},
					budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}
				a := newApp(Config{}, pool, offloadSigner{})
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				defer cancel()
				started := time.Now()
				post, err := a.getPost(ctx, shortcode, nil)
				if !tc.missing {
					if err != nil || post.Username != "dave" {
						t.Fatalf("getPost = %q, %v; want the helper's post", post.Username, err)
					}
					return
				}
				if err == nil || err.Code != errorCodeMediaNotFound || time.Since(started) >= time.Second {
					t.Fatalf("getPost = %v after %s, want media_not_found within a second", err, time.Since(started))
				}
				time.Sleep(cacheSharedWorkTimeout)
				synctest.Wait()
				if entry, ok := a.posts.localGet(shortcode); !ok || entry.err != nil || entry.value.Username != "dave" {
					t.Fatalf("background helper result did not replace the cached miss: %+v", entry)
				}
			})
		})
	}
}
