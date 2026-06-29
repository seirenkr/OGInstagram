package main

import (
	"sync"
	"time"
)

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

	mu      sync.Mutex
	entries map[string]*cacheEntry[V]
	order   []string

	flightMu sync.Mutex
	flight   map[string]*flightCall[V]
}

func newCache[V any](maxEntries int) *cache[V] {
	return &cache[V]{
		maxEntries: maxEntries,
		entries:    map[string]*cacheEntry[V]{},
		flight:     map[string]*flightCall[V]{},
	}
}

func (c *cache[V]) get(key string, meta *fetchMeta, fetch func() (V, time.Duration, *AppError)) (V, *AppError) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && e.expiresAt.After(time.Now()) {
		c.mu.Unlock()
		return e.value, e.err
	}
	c.mu.Unlock()

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

	if meta != nil {
		meta.fetched = true
	}

	value, ttl, err := fetch()
	if err != nil && err.Ephemeral {
		call.err = err
	} else {
		if err != nil {
			ttl = time.Duration(errorCacheSeconds(err.Reason)) * time.Second
		}
		call.entry = c.store(key, &cacheEntry[V]{value: value, err: err, expiresAt: time.Now().Add(ttl)})
		call.err = err
	}

	c.flightMu.Lock()
	delete(c.flight, key)
	c.flightMu.Unlock()
	close(call.done)
	return value, err
}

func (c *cache[V]) store(key string, entry *cacheEntry[V]) *cacheEntry[V] {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	c.entries[key] = entry
	if len(c.entries) <= c.maxEntries {
		return entry
	}
	now := time.Now()
	kept := c.order[:0]
	for _, k := range c.order {
		if len(c.entries) <= c.maxEntries {
			kept = append(kept, k)
			continue
		}
		if e, ok := c.entries[k]; ok && !e.expiresAt.After(now) {
			delete(c.entries, k)
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
	for len(c.entries) > c.maxEntries && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	return entry
}
