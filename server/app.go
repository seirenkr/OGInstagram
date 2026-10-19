package main

import (
	"context"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

type App struct {
	cfg           Config
	pool          *SessionPool
	offloadSigner offloadSigner
	helper        externalHelperClient

	posts    *cache[Post]
	profiles *cache[Profile]
	stories  *cache[Story]
	// Partial posts a flight found while its full sources still run, for
	// requests whose deadline passes first.
	ogPosts *cache[Post]

	fetchSlots chan struct{}

	// Unix nanos until which direct embed requests are skipped (0 = healthy).
	embedPausedUntil atomic.Int64
	ogPausedUntil    atomic.Int64
}

func newApp(cfg Config, pool *SessionPool, signer offloadSigner) *App {
	fetchSlots := make(chan struct{}, maxConcurrentFetches)
	a := &App{
		cfg:           cfg,
		pool:          pool,
		offloadSigner: signer,
		fetchSlots:    fetchSlots,
		posts:         newPersistentCache[Post](cfg.Store, "post", fetchSlots, localPostCacheBytes),
		profiles:      newPersistentCache[Profile](cfg.Store, "profile", fetchSlots, localProfileCacheBytes),
		stories:       newPersistentCache[Story](cfg.Store, "story", fetchSlots, localStoryCacheBytes),
		ogPosts:       newPersistentCache[Post](nil, "og_post", fetchSlots, localOGPostCacheBytes),
	}
	if newExternalHelperClient != nil {
		a.helper = newExternalHelperClient(pool)
	}
	return a
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(ctx context.Context, shortcode string, meta *fetchMeta) (Post, *AppError) {
	// A code that cannot decode to a media id costs paid lookups for a sure miss.
	if pk := shortcodePK(shortcode); !validShortcode(shortcode) || pk == nil || !pk.IsInt64() {
		return Post{}, igErr(404, errorCodeNotFound, "invalid shortcode")
	}
	post, err := a.posts.get(ctx, shortcode, meta, func(fetchCtx context.Context) (Post, time.Duration, bool, *AppError) {
		return a.fetchPost(fetchCtx, shortcode)
	})
	// The flight outlives this request; a partial card beats "temporarily unavailable".
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		if e, ok := a.ogPosts.localGet(shortcode); ok {
			logger(ctx).InfoContext(ctx, "partial post served at deadline")
			return e.value, nil
		}
	}
	return post, err
}

func postCacheTTL(post Post) time.Duration {
	urls := []string{post.ProfilePic}
	for _, att := range post.Attachments {
		urls = append(urls, att.URL, att.Thumbnail)
	}
	return cacheTTLFromURLs(urls...)
}

// helperAsked: the external helper reached Instagram and found no post.
func helperAsked(err *AppError) bool {
	return err != nil && !err.Ephemeral && err.Code != errorCodeConnection && err.Code != errorCodeRateLimited
}

func (a *App) fetchPost(ctx context.Context, shortcode string) (post Post, ttl time.Duration, persist bool, err *AppError) {
	valid := validSourceModel[Post](shortcode)
	// A final ruling can answer before the helper finishes. Keep one bounded
	// attempt available to fill the cache instead of cancelling and restarting it.
	helperCtx, cancelHelper := context.WithCancel(context.WithoutCancel(ctx))
	keepHelper := false
	defer func() {
		if !keepHelper {
			cancelHelper()
		}
	}()
	var rulingAnswered atomic.Bool
	var helperOnce sync.Once
	helperDone := make(chan struct{})
	var helperPost Post
	var helperErr *AppError
	startHelper := func() {
		helperOnce.Do(func() {
			go func() {
				defer close(helperDone)
				defer func() {
					if r := recover(); r != nil {
						logger(helperCtx).ErrorContext(helperCtx, "external helper panicked", "panic", r, "stack", string(debug.Stack()))
						helperErr = ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "external helper panicked")
					}
				}()
				workCtx, cancel := context.WithTimeout(helperCtx, cacheSharedWorkTimeout)
				defer cancel()
				if workCtx.Err() != nil {
					helperErr = contextAppError(workCtx)
					return
				}
				helperPost, helperErr = valid(externalHelperPostImpl(a, workCtx, shortcode))
			}()
		})
	}

	// Crawler Open Graph tags survive some gates; started at most once, they
	// stand in when no full source answers.
	ogCtx, cancelOG := context.WithCancel(ctx)
	defer cancelOG()
	var ogStarted atomic.Bool
	ogDone := make(chan struct{})
	var ogPost Post
	var ogErr *AppError
	startOG := func() {
		if !ogStarted.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer close(ogDone)
			defer func() {
				if r := recover(); r != nil {
					logger(ogCtx).ErrorContext(ogCtx, "og page fetch panicked", "panic", r, "stack", string(debug.Stack()))
					ogErr = ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "og page fetch panicked")
				}
			}()
			if ogPost, ogErr = valid(a.fetchPostOG(ogCtx, shortcode)); ogErr == nil {
				a.ogPosts.put(ogCtx, shortcode, ogPost, min(postCacheTTL(ogPost), partialPostTTL), false)
			}
		}()
	}

	sources := []stagedSource[Post]{
		{name: "post_embed", persist: true, fetch: func(ctx context.Context) (Post, *AppError) {
			return valid(a.fetchPostEmbed(ctx, shortcode))
		}},
		{name: "post_graphql", after: postHedgeDelay, persist: true, fetch: func(ctx context.Context) (Post, *AppError) {
			_, body, gqlErr := a.fetchViaProxy(ctx, webLoggedOutSpec(shortcode))
			if gqlErr != nil {
				return Post{}, gqlErr
			}
			post, perr := parseInstagramPost(body)
			// GraphQL errors the same way for gated, hidden and missing posts; the
			// ruling endpoint can tell "missing" apart in ~0.2s.
			if perr != nil && perr.Code == errorCodeGraphQL {
				missing, answered, ungated := a.mediaMissing(ctx, shortcode)
				rulingAnswered.Store(answered)
				if missing {
					perr = igErr(404, errorCodeMediaNotFound, "This post isn't available.")
					perr.Final = true
				} else if ctx.Err() == nil && gjson.Get(body, "errors.0.code").Int() == graphQLFieldExceptionCode && !ungated {
					startOG()
				}
			}
			return valid(post, perr)
		}},
	}
	if externalHelperPostImpl != nil {
		sources = append(sources,
			stagedSource[Post]{name: "post_helper", after: externalHelperHedgeDelay, persist: true, fetch: func(ctx context.Context) (Post, *AppError) {
				startHelper()
				select {
				case <-helperDone:
					return helperPost, helperErr
				case <-ctx.Done():
					return Post{}, contextAppError(ctx)
				}
			}},
		)
	}
	post, persist, err = stagedFetch(ctx, sources...)
	if err != nil && err.Final {
		// Other sources can still find posts rejected by this ruling, so answer
		// now and keep looking in the background for the next request, but only
		// while a fetch slot is free: this work outlives the flight that held one.
		if externalHelperPostImpl != nil {
			select {
			case a.fetchSlots <- struct{}{}:
				keepHelper = true
				startHelper()
				go func() {
					defer func() { <-a.fetchSlots }()
					defer cancelHelper()
					defer func() {
						if r := recover(); r != nil {
							logger(helperCtx).ErrorContext(helperCtx, "storing helper post panicked", "panic", r, "stack", string(debug.Stack()))
						}
					}()
					<-helperDone
					if helperErr == nil {
						a.storeHelperPost(helperCtx, shortcode, helperPost)
					}
				}()
			default:
			}
		}
		return Post{}, 0, false, err
	}
	if err != nil && ogStarted.Load() {
		<-ogDone
		if ogErr == nil {
			// Cached like the gate verdict below: briefly while the helper could
			// not ask, long once it found nothing, and not at all while it still
			// runs, so the next request asks it again.
			helper := "none"
			ttl = min(postCacheTTL(ogPost), permanentErrorCacheSeconds*time.Second)
			select {
			case <-helperDone:
				if helper = "ok"; helperErr != nil {
					helper = helperErr.errorType()
				}
				if !helperAsked(helperErr) {
					ttl = min(ttl, transientErrorCacheSeconds*time.Second)
				}
			default:
				if externalHelperPostImpl != nil {
					ttl, helper = 0, "running"
				}
			}
			logger(ctx).InfoContext(ctx, "partial post found", "helper", helper, "ttl_s", int(ttl.Seconds()))
			return ogPost, ttl, false, nil
		}
	}
	if err != nil {
		outcome := a.fetchOembed(ctx, shortcode)
		enriched, enrichedErr := oembedVerdict(outcome, shortcode, *err)
		if enriched.Shortcode == "" {
			// oEmbed also 404s posts it hides from logged-out viewers. Its 404 means
			// "gone" only when the ruling could not tell and the external helper had
			// no post either.
			asked := false
			select {
			case <-helperDone:
				if outcome.status == http.StatusNotFound && isTransient(enrichedErr.Code) && !rulingAnswered.Load() &&
					helperErr != nil && helperErr.Status == http.StatusNotFound && helperErr.Code == errorCodeUpstream {
					return Post{}, 0, false, igErr(http.StatusNotFound, errorCodeMediaNotFound, "This post isn't available.")
				}
				asked = helperAsked(helperErr)
			default:
			}
			// The external helper can still fetch gated posts. If it never got to ask,
			// show the gate but cache it briefly so the next request can try again.
			if externalHelperPostImpl != nil && !asked &&
				(enrichedErr.Code == errorCodeRestricted || enrichedErr.Code == errorCodeGeoBlocked) {
				enrichedErr.Code = errorCodeUpstream
			}
			return Post{}, 0, false, enrichedErr
		}
		// After a total failure the proxied oEmbed answer is worth keeping.
		post, persist, err = enriched, true, nil
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	return post, postCacheTTL(post), persist, nil
}

func (a *App) storeHelperPost(ctx context.Context, shortcode string, post Post) {
	ctx, cancel := context.WithTimeout(ctx, cacheSharedWorkTimeout)
	defer cancel()
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	a.posts.putAfterFlight(ctx, shortcode, post, postCacheTTL(post), true)
}
