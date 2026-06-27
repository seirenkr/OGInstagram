package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type cacheEntry struct {
	post      Post
	err       *AppError
	expiresAt time.Time
}

type inflightCall struct {
	done  chan struct{}
	entry *cacheEntry
	err   *AppError
}

type App struct {
	cfg    Config
	pool   *SessionPool
	assets *Assets

	direct *http.Client

	cacheMu sync.Mutex
	cache   map[string]*cacheEntry
	order   []string

	inflightMu sync.Mutex
	inflight   map[string]*inflightCall

	profileMu       sync.Mutex
	profiles        map[string]*profileEntry
	profileOrder    []string
	profileFlightMu sync.Mutex
	profileFlight   map[string]*profileCall
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
		cache:         map[string]*cacheEntry{},
		inflight:      map[string]*inflightCall{},
		profiles:      map[string]*profileEntry{},
		profileFlight: map[string]*profileCall{},
	}
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(shortcode string, meta *fetchMeta) (Post, *AppError) {
	if !validShortcode(shortcode) {
		return Post{}, igErr(404, reasonNotFound, "invalid shortcode")
	}
	entry, err := a.getEntry(shortcode, meta)
	if err != nil {
		return Post{}, err
	}
	return entry.post, nil
}

func (a *App) getEntry(shortcode string, meta *fetchMeta) (*cacheEntry, *AppError) {
	a.cacheMu.Lock()
	if e, ok := a.cache[shortcode]; ok && e.expiresAt.After(time.Now()) {
		a.cacheMu.Unlock()
		return e, e.err
	}
	a.cacheMu.Unlock()

	a.inflightMu.Lock()
	if call, ok := a.inflight[shortcode]; ok {
		a.inflightMu.Unlock()
		<-call.done
		return call.entry, call.err
	}
	call := &inflightCall{done: make(chan struct{})}
	a.inflight[shortcode] = call
	a.inflightMu.Unlock()

	if meta != nil {
		meta.fetched = true
	}

	entry, err := a.fetchEntry(shortcode)
	call.entry, call.err = entry, err

	a.inflightMu.Lock()
	delete(a.inflight, shortcode)
	a.inflightMu.Unlock()
	close(call.done)
	return entry, err
}

func (a *App) fetchEntry(shortcode string) (*cacheEntry, *AppError) {
	post, err := a.fetchPost(shortcode)
	if err != nil {
		if err.Ephemeral {
			return nil, err
		}
		ttl := time.Duration(errorCacheSeconds(err.Reason)) * time.Second
		entry := &cacheEntry{err: err, expiresAt: time.Now().Add(ttl)}
		return a.storeEntry(shortcode, entry), err
	}
	entry := &cacheEntry{post: post, expiresAt: time.Now().Add(time.Duration(a.cfg.CacheTTLSeconds) * time.Second)}
	return a.storeEntry(shortcode, entry), nil
}

func (a *App) fetchPost(shortcode string) (Post, *AppError) {
	body, err := a.raceFetch(docIDSpec(shortcode))
	if err != nil {
		return Post{}, err
	}
	post, perr := parseInstagramPost(body)
	if perr != nil {
		return Post{}, perr
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

func (a *App) storeEntry(shortcode string, entry *cacheEntry) *cacheEntry {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if _, exists := a.cache[shortcode]; !exists {
		a.order = append(a.order, shortcode)
	}
	a.cache[shortcode] = entry
	if len(a.cache) <= maxCacheEntries {
		return entry
	}
	now := time.Now()

	kept := a.order[:0]
	for _, k := range a.order {
		if len(a.cache) <= maxCacheEntries {
			kept = append(kept, k)
			continue
		}
		if e, ok := a.cache[k]; ok && !e.expiresAt.After(now) {
			delete(a.cache, k)
			continue
		}
		kept = append(kept, k)
	}
	a.order = kept
	for len(a.cache) > maxCacheEntries && len(a.order) > 0 {
		oldest := a.order[0]
		a.order = a.order[1:]
		delete(a.cache, oldest)
	}
	return entry
}
