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
		{"captioned_after_proxy_started", "post", "captioned", true},
		{"post_graphql", "post", "graphql", true},
		{"post_external_helper", "post", "helper", true},
		{"post_oembed", "post", "oembed", false}, // a thumbnail stand-in, never persisted
		{"profile_api", "profile", "api", true},
		{"profile_embed", "profile", "embed", false},
		{"story_external_helper", "story", "helper", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := func(status int, body string) *http.Response {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
			}
			var originCalls atomic.Int32
			// Made inside the bubble: channels from outside it never block durably.
			var proxyStarted, proxyDone chan struct{}
			stubDefaultTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				originCalls.Add(1)
				switch tc.source {
				case "captioned":
					select {
					case <-proxyStarted:
						return response(200, trustedEmbedImagePage), nil
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				case "embed":
					return response(200, `{"contextJSON":"{\"context\":{\"username\":\"instagram\",\"profile_pic_url\":\"https://scontent.cdninstagram.com/avatar.jpg\"}}"}`), nil
				default:
					return response(200, "invalid embed"), nil
				}
			}))
			previousPost, previousStory := externalHelperPostImpl, externalHelperStoryImpl
			t.Cleanup(func() {
				externalHelperPostImpl, externalHelperStoryImpl = previousPost, previousStory
			})
			externalHelperPostImpl = func(_ *App, _ context.Context, key string) (Post, *AppError) {
				originCalls.Add(1)
				if tc.source != "helper" {
					return Post{}, igErr(http.StatusBadGateway, errorCodeUpstream, "external helper post unparseable")
				}
				return Post{Shortcode: key, Username: "instagram", Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/image.jpg"}}}, nil
			}
			externalHelperStoryImpl = func(_ *App, _ context.Context, username, id string) (Story, *AppError) {
				originCalls.Add(1)
				return Story{ID: id, Username: username, Media: Attachment{Kind: "image", URL: "https://scontent.cdninstagram.com/story.jpg"}}, nil
			}
			synctest.Test(t, func(t *testing.T) {
				proxyStarted, proxyDone = make(chan struct{}), make(chan struct{})
				proxyClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					originCalls.Add(1)
					embed := strings.HasSuffix(req.URL.Path, "/embed/") || strings.HasSuffix(req.URL.Path, "/embed/captioned/")
					switch {
					case tc.source == "captioned" && embed:
						select {
						case <-proxyStarted:
							return response(200, trustedEmbedImagePage), nil
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					case tc.source == "captioned":
						close(proxyStarted)
						defer close(proxyDone)
						<-req.Context().Done()
						return nil, req.Context().Err()
					case tc.source == "embed" && embed:
						return response(200, `{"contextJSON":"{\"context\":{\"username\":\"instagram\",\"profile_pic_url\":\"https://scontent.cdninstagram.com/avatar.jpg\"}}"}`), nil
					}
					switch tc.source {
					case "graphql":
						return response(200, `{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"DaD8phTyclR","user":{"username":"instagram"},"display_url":"https://scontent.cdninstagram.com/image.jpg","media_type":1}]}}}`), nil
					case "api":
						if req.URL.Path == "/api/graphql/" {
							return response(200, `{"data":{"xig_user_by_username":{"username":"instagram","profile_pic_url":"https://scontent.cdninstagram.com/avatar.jpg"}}}`), nil
						}
					case "oembed":
						if req.URL.Path == "/api/v1/oembed/" {
							return response(200, `{"author_name":"instagram","thumbnail_url":"https://scontent.cdninstagram.com/image.jpg","title":"oEmbed"}`), nil
						}
					}
					return response(200, `{"data":{}}`), nil
				})}
				pool := &SessionPool{
					sessions: []*Session{{client: proxyClient}, {client: proxyClient}, {client: proxyClient}},
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
				synctest.Wait()
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
	for _, tc := range []struct{ name, graphql, wantUser string }{
		{"invalid embed loses to GraphQL", graphql, "bob"},
		{"helper past the request deadline still fills the cache", "", "carol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Swap globals outside the bubble: synctest.Test returns only after the
			// cancelled losers exit, so the restore cannot race them.
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(_ *App, ctx context.Context, key string) (Post, *AppError) {
				select {
				case <-time.After(requestTimeout):
					return Post{Shortcode: key, Username: "carol", Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/x.jpg"}}}, nil
				case <-ctx.Done():
					return Post{}, contextAppError(ctx)
				}
			}
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaTestResponse(r, 200, nil, []byte(invalidEmbed)), nil
			}))
			synctest.Test(t, func(t *testing.T) {
				proxy := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/oembed/" {
						t.Error("oEmbed fetched while a full source could still win")
					}
					if tc.graphql == "" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return mediaTestResponse(r, 200, nil, []byte(tc.graphql)), nil
				})}
				pool := &SessionPool{sessions: []*Session{{client: proxy}}}
				a := newApp(Config{}, pool, offloadSigner{})
				ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
				defer cancel()
				started := time.Now()
				post, err := a.getPost(ctx, "ABC123", nil)
				if late := tc.graphql == ""; late != (err != nil) || !late && post.Username != tc.wantUser {
					t.Fatalf("getPost = %q, %v after %s", post.Username, err, time.Since(started))
				}
				// The helper starts at its hedge delay and runs for requestTimeout.
				time.Sleep(externalHelperHedgeDelay)
				synctest.Wait()
				if entry, ok := a.posts.localGet("ABC123"); !ok || entry.value.Username != tc.wantUser ||
					time.Until(entry.expiresAt) <= transientErrorCacheSeconds*time.Second {
					t.Fatalf("cached %+v, want %s's post with a full TTL", entry, tc.wantUser)
				}
			})
		})
	}
}

func TestConfirmedMissAnswersAtOnceAndStays(t *testing.T) {
	const shortcode = "ABC123"
	gqlError := `{"data":null,"errors":[{"message":"A server error field_exception occured.","code":1675030}]}`
	helperPost := Post{Shortcode: shortcode, Username: "dave",
		Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/x.jpg"}}}
	for _, tc := range []struct {
		name, ruling string
		rulingStatus int
		missing      bool
	}{
		{"ruling and oEmbed misses answer 404 at once", `{"message":"Media cannot be found","status":"fail"}`, http.StatusNotFound, true},
		{"gated post still waits for the helper", `{"title":"This content isn't available to everyone","status":"ok"}`, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := externalHelperPostImpl
			t.Cleanup(func() { externalHelperPostImpl = previous })
			externalHelperPostImpl = func(_ *App, ctx context.Context, _ string) (Post, *AppError) {
				select {
				case <-time.After(1500 * time.Millisecond):
					return helperPost, nil
				case <-ctx.Done():
					return Post{}, contextAppError(ctx)
				}
			}
			stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaTestResponse(r, http.StatusTooManyRequests, nil, nil), nil
			}))
			synctest.Test(t, func(t *testing.T) {
				proxy := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "get_ruling_for_content") {
						return mediaTestResponse(r, tc.rulingStatus, nil, []byte(tc.ruling)), nil
					}
					if r.URL.Path == "/api/v1/oembed/" {
						if tc.missing {
							return mediaTestResponse(r, http.StatusNotFound, nil, []byte(`{"message":"Media cannot be found","status":"fail"}`)), nil
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return mediaTestResponse(r, 200, nil, []byte(gqlError)), nil
				})}
				pool := &SessionPool{sessions: []*Session{{client: proxy}}}
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
				// Gone for both: the helper is cancelled, it could only add a stale copy.
				if entry, ok := a.posts.localGet(shortcode); !ok || entry.err == nil || entry.err.Code != errorCodeMediaNotFound {
					t.Fatalf("the confirmed miss was replaced: %+v", entry)
				}
			})
		})
	}
}
