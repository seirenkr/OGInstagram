package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type App struct {
	cfg    Config
	pool   *SessionPool
	assets *Assets

	direct *http.Client

	posts    *cache[Post]
	profiles *cache[Profile]
}

func newApp(cfg Config, pool *SessionPool, assets *Assets) *App {
	return &App{
		cfg:    cfg,
		pool:   pool,
		assets: assets,
		direct: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
		posts:    newCache[Post](maxCacheEntries),
		profiles: newCache[Profile](maxCacheEntries),
	}
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(shortcode string, meta *fetchMeta) (Post, *AppError) {
	if !validShortcode(shortcode) {
		return Post{}, igErr(404, reasonNotFound, "invalid shortcode")
	}
	return a.posts.get(shortcode, meta, func() (Post, time.Duration, *AppError) {
		post, err := a.fetchPost(shortcode)
		urls := make([]string, 0, len(post.Attachments)*2)
		for _, att := range post.Attachments {
			urls = append(urls, att.URL, att.Thumbnail)
		}
		return post, cacheTTLFromURLs(urls...), err
	})
}

func (a *App) fetchPost(shortcode string) (Post, *AppError) {
	// Proxy-free embed fetch first; the proxied GraphQL fetch joins as the
	// hedge once the embed fails or hasn't answered within fetchHedgeDelay.
	post, err := hedgedPair(
		func() (Post, *AppError) { return a.fetchPostEmbed(shortcode) },
		func() attempt[Post] {
			return func() (Post, *AppError) {
				body, err := a.raceFetch(webLoggedOutSpec(shortcode))
				if err != nil {
					return Post{}, err
				}
				return parseInstagramPost(body)
			}
		},
	)
	if err != nil {
		// Both primary fetches failed; the public oembed endpoint sometimes
		// still resolves (and on failure refines the error card text).
		oe, ok := a.oembedFallback(shortcode, err)
		if !ok {
			return Post{}, err
		}
		post = oe
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	a.flagOversizedVideos(&post)
	return post, nil
}

func (a *App) flagOversizedVideos(post *Post) {
	var wg sync.WaitGroup
	for i := range post.Attachments {
		att := &post.Attachments[i]
		if att.Kind != "video" || att.URL == "" {
			continue
		}
		wg.Add(1)
		go func(att *Attachment) {
			defer wg.Done()
			if a.contentLength(post.Shortcode, att.URL) > maxInlineVideoBytes {
				att.OversizedInline = true
			}
		}(att)
	}
	wg.Wait()
}

func (a *App) contentLength(target, rawURL string) int64 {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), headProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return -1
	}
	req.Header.Set("User-Agent", instagramAppUA)
	resp, err := a.direct.Do(req)
	if err != nil {
		logOutbound("videosize", target, "direct", http.MethodHead, rawURL, started, 502, 0, igErr(502, reasonConnection, err.Error()))
		return -1
	}
	resp.Body.Close()
	logOutbound("videosize", target, "direct", http.MethodHead, rawURL, started, resp.StatusCode, int(resp.ContentLength), nil)
	if resp.StatusCode != 200 {
		return -1
	}
	return resp.ContentLength
}
