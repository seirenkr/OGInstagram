package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	persistentLocalEntryCap = 2048
	errorLocalEntryCap      = 8192

	cacheSharedWorkTimeout   = 10 * time.Second
	cacheEntryMemoryOverhead = 512
	maxCachedMediaItems      = 50
)

type cacheEntry[V any] struct {
	value     V
	err       *AppError
	expiresAt time.Time
	size      int64
}

type flightCall[V any] struct {
	done    chan struct{}
	entry   *cacheEntry[V]
	err     *AppError
	fetched bool
}

type cache[V any] struct {
	maxEntries  int
	maxBytes    int64
	fetchSlots  chan struct{}
	store       *localStore
	storeKind   string
	workTimeout time.Duration

	mu        sync.Mutex
	entries   map[string]*cacheEntry[V]
	order     []string
	usedBytes int64
	// Negative answers keep their own FIFO so they neither evict values nor
	// lose their TTL to value churn.
	errEntries map[string]*cacheEntry[V]
	errOrder   []string

	flightMu sync.Mutex
	flight   map[string]*flightCall[V]
}

type cachePayload[V any] struct {
	Value V `json:"value"`
}

func newPersistentCache[V any](store *localStore, kind string, fetchSlots chan struct{}, maxLocalBytes int64) *cache[V] {
	return &cache[V]{
		maxEntries: persistentLocalEntryCap, maxBytes: maxLocalBytes, fetchSlots: fetchSlots,
		store: store, storeKind: kind, workTimeout: cacheSharedWorkTimeout,
		entries: map[string]*cacheEntry[V]{}, errEntries: map[string]*cacheEntry[V]{}, flight: map[string]*flightCall[V]{},
	}
}

func (c *cache[V]) get(ctx context.Context, key string, meta *fetchMeta, fetch func(context.Context) (V, time.Duration, bool, *AppError)) (V, *AppError) {
	if ctx.Err() != nil {
		var zero V
		return zero, contextAppError(ctx)
	}
	if e, ok := c.localGet(key); ok {
		return e.value, e.err
	}

	c.flightMu.Lock()
	call, exists := c.flight[key]
	if !exists {
		// A flight that ended since the check above stored its entry first.
		if e, ok := c.localGet(key); ok {
			c.flightMu.Unlock()
			return e.value, e.err
		}
		call = &flightCall[V]{done: make(chan struct{})}
		c.flight[key] = call
		go c.runFlight(ctx, key, call, fetch)
	}
	c.flightMu.Unlock()

	select {
	case <-ctx.Done():
		// A deadline while waiting on the origin is an origin outcome, not a hit.
		if meta != nil && ctx.Err() == context.DeadlineExceeded {
			meta.fetched = true
		}
		var zero V
		return zero, contextAppError(ctx)
	case <-call.done:
		if meta != nil {
			meta.fetched = call.fetched
		}
		if call.entry != nil {
			return call.entry.value, call.err
		}
		var zero V
		return zero, call.err
	}
}

// Detached from its caller so one abandoned request cannot kill work other
// waiters share. Bounded by cacheSharedWorkTimeout, longer than requestTimeout,
// so a slow success still warms the cache for the next request.
func (c *cache[V]) runFlight(
	requestCtx context.Context,
	key string,
	call *flightCall[V],
	fetch func(context.Context) (V, time.Duration, bool, *AppError),
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), c.workTimeout)
	defer cancel()
	release := sync.OnceFunc(func() {
		c.flightMu.Lock()
		close(call.done)
		delete(c.flight, key)
		c.flightMu.Unlock()
	})
	defer release()

	if entry, ok := c.persistentGet(ctx, key); ok {
		call.entry = entry
		call.err = entry.err
		c.storeLocal(key, entry)
		return
	}
	call.fetched = true

	select {
	case c.fetchSlots <- struct{}{}:
	case <-ctx.Done():
		call.err = contextAppError(ctx)
		return
	}

	value, ttl, persist, err := func() (value V, ttl time.Duration, persist bool, err *AppError) {
		defer func() { <-c.fetchSlots }()
		defer func() {
			if r := recover(); r != nil {
				logger(ctx).ErrorContext(ctx, "cache fetch panicked", "cache_kind", c.storeKind, "panic", r, "stack", string(debug.Stack()))
				err = ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "upstream fetch failed")
			}
		}()
		return fetch(ctx)
	}()
	if err != nil && ctx.Err() != nil {
		call.err = contextAppError(ctx)
		return
	}
	if cacheErrorIsUncacheable(err) {
		call.err = err
		return
	}
	if err == nil {
		var valid bool
		value, valid = cloneAndValidateCacheValue(key, value)
		if !valid {
			call.err = ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "upstream returned an invalid cache model")
			return
		}
	} else {
		cloned := *err
		cloned.Cause = nil
		err = &cloned
		ttl = time.Duration(errorCacheSeconds(err.Code)) * time.Second
	}
	if ttl <= 0 {
		if err == nil {
			call.entry = &cacheEntry[V]{value: value}
		}
		call.err = err
		return
	}

	entry := &cacheEntry[V]{value: value, err: err, expiresAt: time.Now().Add(ttl)}
	call.entry = entry
	call.err = err
	c.storeLocal(key, entry)
	release()
	if c.store != nil && err == nil && persist {
		// The fetch may have spent most of the shared deadline; the disk write gets its own.
		// Waiters are already released: putAfterFlight only follows a flight
		// that ended in an error, and errors are never persisted.
		putCtx, cancelPut := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancelPut()
		c.persistentPut(putCtx, key, entry)
	}
}

// put stores a value fetched outside get, replacing any cached entry,
// including a cached error.
func (c *cache[V]) put(ctx context.Context, key string, value V, ttl time.Duration, persist bool) {
	value, ok := cloneAndValidateCacheValue(key, value)
	if !ok || ttl <= 0 {
		return
	}
	entry := &cacheEntry[V]{value: value, expiresAt: time.Now().Add(ttl)}
	if c.store != nil && persist {
		c.persistentPut(ctx, key, entry)
	}
	c.storeLocal(key, entry)
}

// putAfterFlight lets a running lookup of key store its verdict first, so that
// verdict cannot replace value.
func (c *cache[V]) putAfterFlight(ctx context.Context, key string, value V, ttl time.Duration, persist bool) {
	c.flightMu.Lock()
	call := c.flight[key]
	c.flightMu.Unlock()
	if call != nil {
		select {
		case <-call.done:
		case <-ctx.Done():
			return
		}
	}
	c.put(ctx, key, value, ttl, persist)
}

func cacheErrorIsUncacheable(err *AppError) bool {
	return err != nil && (err.Ephemeral || err.Status == 499)
}

func (c *cache[V]) persistentGet(ctx context.Context, key string) (*cacheEntry[V], bool) {
	if c.store == nil {
		return nil, false
	}
	body, expiresAt, err := c.store.getModel(ctx, c.storeKind, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		c.logError(ctx, "get", "storage", err)
		return nil, false
	}
	var payload cachePayload[V]
	if err := json.Unmarshal(body, &payload); err != nil {
		c.logError(ctx, "get", "decode", err)
		return nil, false
	}
	value, valid := cloneAndValidateCacheValue(key, payload.Value)
	if !valid {
		c.logError(ctx, "get", "invalid_value", nil)
		return nil, false
	}
	return &cacheEntry[V]{value: value, expiresAt: expiresAt}, true
}

func (c *cache[V]) persistentPut(ctx context.Context, key string, entry *cacheEntry[V]) {
	body, err := json.Marshal(cachePayload[V]{Value: entry.value})
	if err != nil {
		c.logError(ctx, "put", "encode", err)
		return
	}
	if err = c.store.putModel(ctx, c.storeKind, key, body, entry.expiresAt); err != nil {
		c.logError(ctx, "put", "storage", err)
	}
}

func (c *cache[V]) logError(ctx context.Context, operation, errorType string, err error) {
	attrs := []any{"operation", operation, "cache_kind", c.storeKind, "error_type", errorType}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	logger(ctx).WarnContext(ctx, "cache request failed", attrs...)
}

func (c *cache[V]) localGet(key string) (*cacheEntry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		entry, ok = c.errEntries[key]
	}
	if !ok {
		return nil, false
	}
	if entry.expiresAt.After(time.Now()) {
		return entry, true
	}
	c.removeLocalLocked(key, entry)
	return nil, false
}

func (c *cache[V]) removeLocalLocked(key string, entry *cacheEntry[V]) {
	entries, order := c.entries, &c.order
	if entry.err != nil {
		entries, order = c.errEntries, &c.errOrder
	} else {
		c.usedBytes = max(c.usedBytes-entry.size, 0)
	}
	delete(entries, key)
	if i := slices.Index(*order, key); i >= 0 {
		*order = slices.Delete(*order, i, i+1)
	}
}

func (c *cache[V]) storeLocal(key string, entry *cacheEntry[V]) {
	if entry.err == nil {
		entry.size = cacheEntryMemorySize(key, entry)
		if entry.size <= 0 || entry.size > c.maxBytes {
			return
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, old := range []*cacheEntry[V]{c.entries[key], c.errEntries[key]} {
		if old != nil {
			c.removeLocalLocked(key, old)
		}
	}
	key = strings.Clone(key)
	if entry.err != nil {
		c.errEntries[key] = entry
		c.errOrder = append(c.errOrder, key)
		if len(c.errOrder) > errorLocalEntryCap {
			delete(c.errEntries, c.errOrder[0])
			c.errOrder = c.errOrder[1:]
		}
		return
	}
	c.entries[key] = entry
	c.order = append(c.order, key)
	c.usedBytes += entry.size

	for (c.usedBytes > c.maxBytes || len(c.entries) > c.maxEntries) && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.entries[oldest]; ok {
			c.usedBytes -= e.size
			delete(c.entries, oldest)
		}
	}
}

// validSourceModel rejects a parsed model the cache would refuse, so an invalid
// result loses the staged race instead of blocking the fallback sources.
func validSourceModel[V any](key string) func(V, *AppError) (V, *AppError) {
	return func(v V, err *AppError) (V, *AppError) {
		if err == nil {
			if _, ok := cloneAndValidateCacheValue(key, v); !ok {
				var zero V
				return zero, igErr(http.StatusBadGateway, errorCodeUpstream, "upstream returned an invalid cache model")
			}
		}
		return v, err
	}
}

// ponytail: a cold fetch round-trips the model through JSON 4-6 times. jsonClone
// must stay to break aliasing into upstream bodies; if CPU profiles ever show it,
// validate a shallow copy in validSourceModel instead.
func cloneAndValidateCacheValue[V any](key string, value V) (V, bool) {
	value = jsonClone(value)
	switch typed := any(value).(type) {
	case Post:
		typed.ProfilePic = normalizeCDNHost(typed.ProfilePic)
		for i := range typed.Attachments {
			typed.Attachments[i] = normalizeCachedAttachment(typed.Attachments[i])
		}
		value = any(typed).(V)
		return value, validCachedPost(key, typed)
	case Profile:
		typed.ProfilePic = normalizeCDNHost(typed.ProfilePic)
		for i := range typed.RecentMedia {
			typed.RecentMedia[i].Thumbnail = normalizeCDNHost(typed.RecentMedia[i].Thumbnail)
		}
		value = any(typed).(V)
		return value, validCachedProfile(key, typed)
	case Story:
		typed.ProfilePic = normalizeCDNHost(typed.ProfilePic)
		typed.Media = normalizeCachedAttachment(typed.Media)
		value = any(typed).(V)
		return value, validCachedStory(key, typed)
	default:
		return value, true
	}
}

func normalizeCachedAttachment(attachment Attachment) Attachment {
	attachment.URL = normalizeCDNHost(attachment.URL)
	attachment.Thumbnail = normalizeCDNHost(attachment.Thumbnail)
	return attachment
}

func jsonClone[V any](value V) V {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned V
	if json.Unmarshal(encoded, &cloned) != nil {
		return value
	}
	return cloned
}

func validCachedPost(key string, post Post) bool {
	if !validShortcode(post.Shortcode) || post.Shortcode != key ||
		!validUsername(post.Username) || len(post.Attachments) == 0 ||
		len(post.Attachments) > maxCachedMediaItems || !validOptionalCachedURL(post.ProfilePic) {
		return false
	}
	for _, attachment := range post.Attachments {
		if !validCachedAttachment(attachment) {
			return false
		}
	}
	return true
}

func validCachedProfile(key string, profile Profile) bool {
	if !validUsername(profile.Username) || !strings.EqualFold(profile.Username, key) ||
		!validOptionalCachedURL(profile.ProfilePic) || len(profile.RecentMedia) > profileGalleryMax ||
		profile.FollowerCount < 0 || profile.FollowingCount < 0 || profile.MediaCount < 0 {
		return false
	}
	for _, media := range profile.RecentMedia {
		if media.Thumbnail == "" || !validCachedURL(media.Thumbnail) ||
			!validCachedDimensions(media.Width, media.Height) {
			return false
		}
	}
	return true
}

func validCachedStory(key string, story Story) bool {
	username, id, found := strings.Cut(key, "/")
	return found && validUsername(story.Username) && strings.EqualFold(story.Username, username) &&
		validStoryID(story.ID) && story.ID == id && validOptionalCachedURL(story.ProfilePic) &&
		validCachedAttachment(story.Media)
}

func validCachedAttachment(attachment Attachment) bool {
	if attachment.Kind != "image" && attachment.Kind != "video" {
		return false
	}
	return validCachedURL(attachment.URL) && validOptionalCachedURL(attachment.Thumbnail) &&
		validCachedDimensions(attachment.Width, attachment.Height)
}

func validCachedDimensions(width, height int) bool {
	const maxDimension = 100_000
	return width >= 0 && width <= maxDimension && height >= 0 && height <= maxDimension
}

func validOptionalCachedURL(raw string) bool {
	return raw == "" || validCachedURL(raw)
}

func validCachedURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && trustedMediaURL(parsed)
}

func cacheEntryMemorySize[V any](key string, entry *cacheEntry[V]) int64 {
	size := int64(cacheEntryMemoryOverhead) + int64(len(key))*2
	if encoded, err := json.Marshal(entry.value); err == nil {
		size += int64(len(encoded)) * 2
	}
	return size
}
