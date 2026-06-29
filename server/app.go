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
		return post, time.Duration(a.cfg.CacheTTLSeconds) * time.Second, err
	})
}

func (a *App) fetchPost(shortcode string) (Post, *AppError) {

	post, err := a.fetchPostWith(webLoggedOutSpec(shortcode))
	if err != nil {
		return Post{}, err
	}
	a.flagOversizedVideos(&post)
	return post, nil
}

func (a *App) fetchPostWith(spec gqlSpec) (Post, *AppError) {
	body, err := a.raceFetch(spec)
	if err != nil {
		return Post{}, err
	}
	return parseInstagramPost(body)
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
			if a.contentLength(att.URL) > maxInlineVideoBytes {
				att.OversizedInline = true
			}
		}(att)
	}
	wg.Wait()
}

func (a *App) contentLength(rawURL string) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return -1
	}
	req.Header.Set("User-Agent", instagramAppUA)
	resp, err := a.direct.Do(req)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return -1
	}
	return resp.ContentLength
}
