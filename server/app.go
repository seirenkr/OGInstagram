package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type App struct {
	cfg           Config
	pool          *SessionPool
	offloadSigner offloadSigner

	posts    *cache[Post]
	profiles *cache[Profile]
	stories  *cache[Story]
}

func newApp(cfg Config, pool *SessionPool, signer offloadSigner) *App {
	fetchSlots := make(chan struct{}, maxConcurrentFetches)
	return &App{
		cfg:           cfg,
		pool:          pool,
		offloadSigner: signer,
		posts:         newPersistentCache[Post](cfg.Store, "post", fetchSlots, localPostCacheBytes),
		profiles:      newPersistentCache[Profile](cfg.Store, "profile", fetchSlots, localProfileCacheBytes),
		stories:       newPersistentCache[Story](cfg.Store, "story", fetchSlots, localStoryCacheBytes),
	}
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(ctx context.Context, shortcode string, meta *fetchMeta) (Post, *AppError) {
	if !validShortcode(shortcode) {
		return Post{}, igErr(404, errorCodeNotFound, "invalid shortcode")
	}
	return a.posts.get(ctx, shortcode, meta, func(fetchCtx context.Context) (Post, time.Duration, bool, *AppError) {
		post, persist, degraded, err := a.fetchPost(fetchCtx, shortcode)
		urls := []string{post.ProfilePic}
		for _, att := range post.Attachments {
			urls = append(urls, att.URL, att.Thumbnail)
		}
		ttl := cacheTTLFromURLs(urls...)
		if degraded {
			ttl = min(ttl, transientErrorCacheSeconds*time.Second)
		}
		return post, ttl, persist, err
	})
}

// degraded reports an oEmbed card served because the full sources were still
// running; it is cached briefly so a later full fetch replaces it.
func (a *App) fetchPost(ctx context.Context, shortcode string) (post Post, persist, degraded bool, err *AppError) {
	oembedCtx, cancelOembed := context.WithCancel(ctx)
	defer cancelOembed()
	stagedCtx, cancelStaged := context.WithCancel(ctx)
	defer cancelStaged()
	started := time.Now()
	var cutEarly atomic.Bool
	oembedCh := make(chan oembedOutcome, 1)
	var oembedOnce sync.Once
	startOembed := func() {
		oembedOnce.Do(func() {
			go func() {
				outcome := a.fetchOembed(oembedCtx, shortcode)
				oembedCh <- outcome
				if _, verr := oembedVerdict(outcome, shortcode, AppError{}); verr != nil {
					return
				}
				// A usable degraded card is ready: end the staged race in time to serve it.
				select {
				case <-time.After(time.Until(started.Add(oembedFallbackAt))):
					cutEarly.Store(true)
					cancelStaged()
				case <-stagedCtx.Done():
				}
			}()
		})
	}
	valid := validSourceModel[Post](shortcode)
	oembedTimer := time.AfterFunc(oembedHedgeDelay, startOembed)
	defer oembedTimer.Stop()

	post, persist, err = stagedFetch(stagedCtx,
		stagedSource[Post]{name: "post_embed", fetch: func(ctx context.Context) (Post, *AppError) {
			return valid(a.fetchPostEmbed(ctx, shortcode))
		}},
		stagedSource[Post]{name: "post_graphql", after: postHedgeDelay, persist: true, fetch: func(ctx context.Context) (Post, *AppError) {
			_, body, gqlErr := a.fetchViaProxy(ctx, webLoggedOutSpec(shortcode))
			if gqlErr != nil {
				return Post{}, gqlErr
			}
			return valid(parseInstagramPost(body))
		}},
		stagedSource[Post]{name: "post_helper", after: externalHelperHedgeDelay, persist: true, fetch: func(ctx context.Context) (Post, *AppError) {
			helperPost, ok := externalHelperPostImpl(a, ctx, shortcode)
			if !ok {
				return Post{}, ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "external helper had no post")
			}
			return valid(helperPost, nil)
		}},
	)
	if err != nil {
		startOembed()
		var outcome oembedOutcome
		// A finished oEmbed wins over an expired ctx: at the deadline both
		// channels are ready, and select would discard the answer half the time.
		select {
		case outcome = <-oembedCh:
		default:
			select {
			case outcome = <-oembedCh:
			case <-ctx.Done():
			}
		}
		enriched, enrichedErr := oembedVerdict(outcome, shortcode, *err)
		if enriched.Shortcode == "" {
			return Post{}, false, false, enrichedErr
		}
		post, err = enriched, nil
		// After a total failure the proxied oEmbed answer is worth keeping; a card
		// taken while full sources were still running is only a stopgap.
		degraded = cutEarly.Load()
		persist = !degraded
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	return post, persist, degraded, nil
}
