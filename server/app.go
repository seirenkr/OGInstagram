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
	post, err := raceEmbedFirst(
		func() (Post, *AppError) { return a.fetchPostEmbed(shortcode) },
		func() (Post, *AppError) { return a.fetchPostWith(webLoggedOutSpec(shortcode)) },
	)
	if err != nil {
		return Post{}, err
	}
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	a.flagOversizedVideos(&post)
	return post, nil
}

// raceEmbedFirst starts the proxy-free embed fetch immediately and adds the
// proxied GraphQL fetch once the embed fails or hasn't answered within
// fetchHedgeDelay; the first success wins. Worst case is bounded by a single
// fetchTimeout after the last launch instead of the sum of serial attempts.
func raceEmbedFirst(embed, gql func() (Post, *AppError)) (Post, *AppError) {
	type result struct {
		post Post
		err  *AppError
	}
	results := make(chan result, 2)
	launch := func(f func() (Post, *AppError)) {
		go func() {
			p, err := f()
			results <- result{p, err}
		}()
	}
	launch(embed)
	pending := 1
	gqlLaunched := false
	launchGQL := func() {
		if !gqlLaunched {
			gqlLaunched = true
			pending++
			launch(gql)
		}
	}

	timer := time.NewTimer(fetchHedgeDelay)
	defer timer.Stop()

	var lastErr *AppError
	for pending > 0 {
		select {
		case <-timer.C:
			launchGQL()
		case r := <-results:
			pending--
			if r.err == nil {
				return r.post, nil
			}
			// Prefer a permanent error (real 404) over a transient one.
			if lastErr == nil || (isTransient(lastErr.Reason) && !isTransient(r.err.Reason)) {
				lastErr = r.err
			}
			launchGQL()
		}
	}
	return Post{}, lastErr
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
