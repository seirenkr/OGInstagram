package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxModelCacheBodyBytes = 1_900_000

type cacheEntry[V any] struct {
	value     V
	err       *AppError
	expiresAt time.Time
}

type flightCall[V any] struct {
	done  chan struct{}
	entry *cacheEntry[V]
	err   *AppError
}

type cache[V any] struct {
	maxEntries int
	fetchSlots chan struct{}
	remoteURL  string
	remoteKey  string
	client     *http.Client

	mu      sync.Mutex
	entries map[string]*cacheEntry[V]
	order   []string

	flightMu sync.Mutex
	flight   map[string]*flightCall[V]
}

type cachePayload[V any] struct {
	Value V         `json:"value"`
	Error *AppError `json:"error,omitempty"`
}

func newCache[V any](maxEntries int, fetchSlots chan struct{}) *cache[V] {
	c := &cache[V]{
		maxEntries: maxEntries,
		fetchSlots: fetchSlots,
		flight:     map[string]*flightCall[V]{},
	}
	if maxEntries > 0 {
		c.entries = map[string]*cacheEntry[V]{}
	}
	return c
}

func newPersistentCache[V any](remoteURL, remoteKey string, fetchSlots chan struct{}) *cache[V] {
	c := newCache[V](0, fetchSlots)
	c.remoteURL = strings.TrimRight(remoteURL, "/")
	c.remoteKey = remoteKey
	c.client = &http.Client{Timeout: time.Second}
	return c
}

func (c *cache[V]) get(ctx context.Context, key string, meta *fetchMeta, fetch func() (V, time.Duration, *AppError)) (V, *AppError) {
	if c.maxEntries > 0 {
		c.mu.Lock()
		if e, ok := c.entries[key]; ok && e.expiresAt.After(time.Now()) {
			c.mu.Unlock()
			return e.value, e.err
		}
		c.mu.Unlock()
	}

	c.flightMu.Lock()
	if call, ok := c.flight[key]; ok {
		c.flightMu.Unlock()
		<-call.done
		if call.entry != nil {
			return call.entry.value, call.err
		}
		var zero V
		return zero, call.err
	}
	call := &flightCall[V]{done: make(chan struct{})}
	c.flight[key] = call
	c.flightMu.Unlock()
	defer func() {
		c.flightMu.Lock()
		delete(c.flight, key)
		c.flightMu.Unlock()
		close(call.done)
	}()

	if entry, ok := c.remoteGet(ctx, key); ok {
		call.entry = entry
		call.err = entry.err
		return entry.value, entry.err
	}

	if meta != nil {
		meta.fetched = true
	}

	c.fetchSlots <- struct{}{}
	value, ttl, err := func() (V, time.Duration, *AppError) {
		defer func() { <-c.fetchSlots }()
		return fetch()
	}()
	if err != nil && err.Ephemeral {
		call.err = err
	} else {
		if err != nil {
			ttl = time.Duration(errorCacheSeconds(err.Reason)) * time.Second
		}
		entry := &cacheEntry[V]{value: value, err: err, expiresAt: time.Now().Add(ttl)}
		call.entry = entry
		call.err = err
		if c.remoteURL != "" {
			c.remotePut(ctx, key, entry)
		} else if c.maxEntries > 0 {
			c.store(key, entry)
		}
	}
	return value, err
}

func (c *cache[V]) remoteGet(ctx context.Context, key string) (*cacheEntry[V], bool) {
	if c.remoteURL == "" {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.remoteURL+"?key="+url.QueryEscape(c.remoteKey+":"+key), nil)
	if err != nil {
		c.logError(ctx, "get", "request", 0, err)
		return nil, false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.logError(ctx, "get", "connection", 0, err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode != http.StatusNotFound {
			c.logError(ctx, "get", "status", resp.StatusCode, nil)
		}
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelCacheBodyBytes+1))
	if err != nil {
		c.logError(ctx, "get", "body_read", resp.StatusCode, err)
		return nil, false
	}
	if len(body) > maxModelCacheBodyBytes {
		c.logError(ctx, "get", "body_too_large", resp.StatusCode, nil)
		return nil, false
	}
	var payload cachePayload[V]
	if err := json.Unmarshal(body, &payload); err != nil {
		c.logError(ctx, "get", "decode", resp.StatusCode, err)
		return nil, false
	}
	return &cacheEntry[V]{value: payload.Value, err: payload.Error}, true
}

func (c *cache[V]) known(ctx context.Context, key string) bool {
	if c.remoteURL == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		c.remoteURL+"?key="+url.QueryEscape(c.remoteKey+":"+key), nil)
	if err != nil {
		c.logError(ctx, "known", "request", 0, err)
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.logError(ctx, "known", "connection", 0, err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		c.logError(ctx, "known", "status", resp.StatusCode, nil)
	}
	return resp.StatusCode == http.StatusNoContent
}

func (c *cache[V]) remotePut(ctx context.Context, key string, entry *cacheEntry[V]) {
	body, err := json.Marshal(cachePayload[V]{Value: entry.value, Error: entry.err})
	if err != nil {
		c.logError(ctx, "put", "encode", 0, err)
		return
	}
	if len(body) > maxModelCacheBodyBytes {
		c.logError(ctx, "put", "body_too_large", 0, nil)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.remoteURL+"?key="+url.QueryEscape(c.remoteKey+":"+key), bytes.NewReader(body))
	if err != nil {
		c.logError(ctx, "put", "request", 0, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cache-Expires", strconv.FormatInt(entry.expiresAt.UnixMilli(), 10))
	if entry.err == nil {
		req.Header.Set("X-Cache-Known", "1")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.logError(ctx, "put", "connection", 0, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		c.logError(ctx, "put", "status", resp.StatusCode, nil)
	}
}

func (c *cache[V]) logError(ctx context.Context, operation, reason string, status int, err error) {
	attrs := []any{"event", "model_cache_error", "operation", operation, "kind", c.remoteKey,
		"reason", reason, "status", status}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	logger(ctx).Warn("model cache request failed", attrs...)
}

// store evicts in FIFO order; refreshed keys keep their original position.
// Good enough for the small in-memory caches this backs.
func (c *cache[V]) store(key string, entry *cacheEntry[V]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	c.entries[key] = entry

	for len(c.entries) > c.maxEntries && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}
