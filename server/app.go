package main

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type App struct {
	cfg  Config
	pool *SessionPool

	direct *http.Client

	posts      *cache[Post]
	profiles   *cache[Profile]
	stories    *cache[Story]
	videoSizes *cache[int64]
}

func newApp(cfg Config, pool *SessionPool) *App {
	fetchSlots := make(chan struct{}, maxConcurrentFetches)
	return &App{
		cfg:  cfg,
		pool: pool,
		direct: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				MaxConnsPerHost:     maxConcurrentFetches,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
		posts:      newPersistentCache[Post](cfg.ModelCacheURL, "post", fetchSlots),
		profiles:   newPersistentCache[Profile](cfg.ModelCacheURL, "profile", fetchSlots),
		stories:    newPersistentCache[Story](cfg.ModelCacheURL, "story", fetchSlots),
		videoSizes: newCache[int64](maxCacheEntries, fetchSlots),
	}
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(ctx context.Context, shortcode string, meta *fetchMeta) (Post, *AppError) {
	if !validShortcode(shortcode) {
		return Post{}, igErr(404, reasonNotFound, "invalid shortcode")
	}
	return a.posts.get(ctx, shortcode, meta, func() (Post, time.Duration, *AppError) {
		post, err := a.fetchPost(ctx, shortcode)
		urls := make([]string, 0, len(post.Attachments)*2)
		for _, att := range post.Attachments {
			urls = append(urls, att.URL, att.Thumbnail)
		}
		return post, cacheTTLFromURLs(urls...), err
	})
}

func (a *App) fetchPost(ctx context.Context, shortcode string) (Post, *AppError) {
	post, err := a.fetchPostEmbed(ctx, shortcode)
	if err != nil {
		body, gqlErr := a.raceFetch(ctx, webLoggedOutSpec(shortcode))
		if gqlErr == nil {
			post, gqlErr = parseInstagramPost(body)
		}
		err = gqlErr
	}
	if err != nil {

		oembedErr := *err
		var ok bool
		post, ok = concurrentPostFallbacks(ctx,
			func(ctx context.Context) (Post, bool) {
				if externalHelperPostFallbackFn == nil {
					return Post{}, false
				}
				return externalHelperPostFallbackFn(a, ctx, shortcode)
			},
			func(ctx context.Context) (Post, bool) { return a.oembedFallback(ctx, shortcode, &oembedErr) },
		)
		if !ok {
			return Post{}, &oembedErr
		}
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	return post, nil
}

func concurrentPostFallbacks(parent context.Context, preferred, backup func(context.Context) (Post, bool)) (Post, bool) {
	type result struct {
		post Post
		ok   bool
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	backupResult := make(chan result, 1)
	go func() {
		post, ok := backup(ctx)
		backupResult <- result{post, ok}
	}()
	if post, ok := preferred(ctx); ok {
		return post, true
	}
	r := <-backupResult
	return r.post, r.ok
}

func (a *App) contentLength(parent context.Context, target, rawURL string) int64 {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, headProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return -1
	}
	req.Header.Set("User-Agent", instagramAppUA)
	resp, err := a.direct.Do(req)
	if err != nil {
		logOutbound(ctx, "videosize", target, "direct", http.MethodHead, rawURL, started, 502, 0, igErr(502, reasonConnection, err.Error()))
		return -1
	}
	resp.Body.Close()
	logOutbound(ctx, "videosize", target, "direct", http.MethodHead, rawURL, started, resp.StatusCode, int(resp.ContentLength), nil)
	if resp.StatusCode != 200 {
		return -1
	}
	return resp.ContentLength
}

func (a *App) cachedContentLength(ctx context.Context, target, rawURL string) int64 {
	size, _ := a.videoSizes.get(ctx, strings.Clone(rawURL), nil, func() (int64, time.Duration, *AppError) {
		return a.contentLength(ctx, target, rawURL), cacheTTLFromURLs(rawURL), nil
	})
	return size
}
