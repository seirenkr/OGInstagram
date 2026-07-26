package main

import (
	"context"
	"net/http"
	"sync"
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
		posts:         newPersistentCache[Post](cfg.CacheURL, "post", fetchSlots, localPostCacheBytes),
		profiles:      newPersistentCache[Profile](cfg.CacheURL, "profile", fetchSlots, localProfileCacheBytes),
		stories:       newPersistentCache[Story](cfg.CacheURL, "story", fetchSlots, localStoryCacheBytes),
	}
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(ctx context.Context, shortcode string, meta *fetchMeta) (Post, *AppError) {
	if !validShortcode(shortcode) {
		return Post{}, igErr(404, errorCodeNotFound, "invalid shortcode")
	}
	return a.posts.get(ctx, shortcode, meta, func(fetchCtx context.Context) (Post, time.Duration, *AppError) {
		post, err := a.fetchPost(fetchCtx, shortcode)
		urls := []string{post.ProfilePic}
		for _, att := range post.Attachments {
			urls = append(urls, att.URL, att.Thumbnail)
		}
		return post, cacheTTLFromURLs(urls...), err
	})
}

func (a *App) fetchPost(ctx context.Context, shortcode string) (Post, *AppError) {
	oembedCtx, cancelOembed := context.WithCancel(ctx)
	defer cancelOembed()
	oembedCh := make(chan oembedOutcome, 1)
	var oembedOnce sync.Once
	startOembed := func() {
		oembedOnce.Do(func() {
			go func() { oembedCh <- a.fetchOembed(oembedCtx, shortcode) }()
		})
	}
	oembedTimer := time.AfterFunc(oembedHedgeDelay, startOembed)
	defer oembedTimer.Stop()

	post, err := stagedFetch(ctx,
		stagedSource[Post]{fetch: func(ctx context.Context) (Post, *AppError) {
			return a.fetchPostEmbed(ctx, shortcode)
		}},
		stagedSource[Post]{after: postHedgeDelay, fetch: func(ctx context.Context) (Post, *AppError) {
			_, body, gqlErr := a.fetchViaProxy(ctx, webLoggedOutSpec(shortcode))
			if gqlErr != nil {
				return Post{}, gqlErr
			}
			return parseInstagramPost(body)
		}},
		stagedSource[Post]{after: externalHelperHedgeDelay, fetch: func(ctx context.Context) (Post, *AppError) {
			helperPost, ok := a.externalHelperPost(ctx, shortcode)
			if !ok {
				return Post{}, ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "external helper had no post")
			}
			return helperPost, nil
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
			return Post{}, enrichedErr
		}
		post = enriched
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	return post, nil
}
