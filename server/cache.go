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

const (
	maxCacheValueBytes      = 1_900_000
	persistentLocalEntryCap = 2048

	cacheSharedWorkTimeout   = requestTimeout
	cacheEntryMemoryOverhead = 512
	maxCachedMediaItems      = 50
)

var sharedCacheHTTPClient = &http.Client{Timeout: 2 * time.Second}

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
	remoteURL   string
	remoteKey   string
	client      *http.Client
	workTimeout time.Duration

	mu        sync.Mutex
	entries   map[string]*cacheEntry[V]
	order     []string
	usedBytes int64

	flightMu sync.Mutex
	flight   map[string]*flightCall[V]
}

type cachePayload[V any] struct {
	Value V `json:"value"`
}

func newCache[V any](maxEntries int, maxBytes int64, fetchSlots chan struct{}) *cache[V] {
	c := &cache[V]{
		maxEntries:  maxEntries,
		maxBytes:    maxBytes,
		fetchSlots:  fetchSlots,
		workTimeout: cacheSharedWorkTimeout,
		flight:      map[string]*flightCall[V]{},
	}
	if maxEntries > 0 {
		c.entries = map[string]*cacheEntry[V]{}
	}
	return c
}

func newPersistentCache[V any](remoteURL, remoteKey string, fetchSlots chan struct{}, maxLocalBytes int64) *cache[V] {
	c := newCache[V](0, 0, fetchSlots)
	c.remoteURL = strings.TrimRight(remoteURL, "/")
	c.remoteKey = remoteKey
	c.client = sharedCacheHTTPClient
	c.maxBytes = maxLocalBytes
	if maxLocalBytes > 0 {
		c.maxEntries = persistentLocalEntryCap
		c.entries = map[string]*cacheEntry[V]{}
	}
	return c
}

func (c *cache[V]) get(ctx context.Context, key string, meta *fetchMeta, fetch func(context.Context) (V, time.Duration, *AppError)) (V, *AppError) {
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
		call = &flightCall[V]{done: make(chan struct{})}
		c.flight[key] = call
		go c.runFlight(ctx, key, call, fetch)
	}
	c.flightMu.Unlock()

	select {
	case <-ctx.Done():
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
// waiters share; still bounded by the same requestTimeout.
func (c *cache[V]) runFlight(
	requestCtx context.Context,
	key string,
	call *flightCall[V],
	fetch func(context.Context) (V, time.Duration, *AppError),
) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), c.workTimeout)
	defer cancel()
	defer func() {
		c.flightMu.Lock()
		close(call.done)
		delete(c.flight, key)
		c.flightMu.Unlock()
	}()

	if entry, ok := c.remoteGet(ctx, key); ok {
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

	value, ttl, err := func() (V, time.Duration, *AppError) {
		defer func() { <-c.fetchSlots }()
		return fetch(ctx)
	}()
	if ctx.Err() != nil {
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
		cloned := jsonClone(*err)
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
	if c.remoteURL != "" && err == nil {
		c.remotePutAsync(ctx, key, entry)
	}
	c.storeLocal(key, entry)
}

func cacheErrorIsUncacheable(err *AppError) bool {
	return err != nil && (err.Ephemeral || err.Status == 499)
}

func (c *cache[V]) remoteGet(ctx context.Context, key string) (entry *cacheEntry[V], ok bool) {
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCacheValueBytes+1))
	if err != nil {
		c.logError(ctx, "get", "body_read", resp.StatusCode, err)
		return nil, false
	}
	if len(body) > maxCacheValueBytes {
		c.logError(ctx, "get", "body_too_large", resp.StatusCode, nil)
		return nil, false
	}
	var payload cachePayload[V]
	if err := json.Unmarshal(body, &payload); err != nil {
		c.logError(ctx, "get", "decode", resp.StatusCode, err)
		return nil, false
	}
	value, valid := cloneAndValidateCacheValue(key, payload.Value)
	if !valid {
		c.logError(ctx, "get", "invalid_value", resp.StatusCode, nil)
		return nil, false
	}

	var expiresAt time.Time
	if raw := strings.TrimSpace(resp.Header.Get("X-Cache-Expires")); raw != "" {
		millis, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			c.logError(ctx, "get", "invalid_expiry", resp.StatusCode, parseErr)
			return nil, false
		}
		expiresAt = time.UnixMilli(millis)
		if !expiresAt.After(time.Now()) {
			c.logError(ctx, "get", "expired_value", resp.StatusCode, nil)
			return nil, false
		}
	}
	return &cacheEntry[V]{value: value, expiresAt: expiresAt}, true
}

func (c *cache[V]) remotePutAsync(ctx context.Context, key string, entry *cacheEntry[V]) {
	reqID, _ := ctx.Value(requestIDKey{}).(string)
	bg, cancel := context.WithTimeout(context.WithValue(context.Background(), requestIDKey{}, reqID), c.client.Timeout)
	go func() {
		defer cancel()
		c.remotePut(bg, key, entry)
	}()
}

func (c *cache[V]) remotePut(ctx context.Context, key string, entry *cacheEntry[V]) (ok bool) {
	body, err := json.Marshal(cachePayload[V]{Value: entry.value})
	if err != nil {
		c.logError(ctx, "put", "encode", 0, err)
		return false
	}
	if len(body) > maxCacheValueBytes {
		c.logError(ctx, "put", "body_too_large", 0, nil)
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.remoteURL+"?key="+url.QueryEscape(c.remoteKey+":"+key), bytes.NewReader(body))
	if err != nil {
		c.logError(ctx, "put", "request", 0, err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cache-Expires", strconv.FormatInt(entry.expiresAt.UnixMilli(), 10))
	resp, err := c.client.Do(req)
	if err != nil {
		c.logError(ctx, "put", "connection", 0, err)
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		c.logError(ctx, "put", "status", resp.StatusCode, nil)
		return false
	}
	return true
}

func (c *cache[V]) logError(ctx context.Context, operation, errorType string, status int, err error) {
	attrs := []any{"event", "cache_request_failed", "operation", operation, "cache_kind", c.remoteKey,
		"error.type", errorType}
	if status != 0 {
		attrs = append(attrs, "http.response.status_code", status)
	}
	if err != nil {
		attrs = append(attrs, "exception.message", err.Error())
	}
	logger(ctx).WarnContext(ctx, "cache request failed", attrs...)
}

func (c *cache[V]) storeLocal(key string, entry *cacheEntry[V]) {
	if c.maxBytes > 0 {
		c.storeBounded(key, entry)
	}
}

func (c *cache[V]) localGet(key string) (*cacheEntry[V], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
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
	delete(c.entries, key)
	c.usedBytes -= entry.size
	if c.usedBytes < 0 {
		c.usedBytes = 0
	}
	for i, orderedKey := range c.order {
		if orderedKey == key {
			copy(c.order[i:], c.order[i+1:])
			c.order = c.order[:len(c.order)-1]
			return
		}
	}
}

func (c *cache[V]) storeBounded(key string, entry *cacheEntry[V]) {
	size := cacheEntryMemorySize(key, entry)
	if size <= 0 || size > c.maxBytes {
		return
	}
	entry.size = size

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, exists := c.entries[key]; exists {
		c.usedBytes -= old.size
	} else {
		key = strings.Clone(key)
		c.order = append(c.order, key)
	}
	c.entries[key] = entry
	c.usedBytes += size

	for (c.usedBytes > c.maxBytes || len(c.entries) > c.maxEntries) && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.entries[oldest]; ok {
			c.usedBytes -= e.size
			delete(c.entries, oldest)
		}
	}
}

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
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func cacheEntryMemorySize[V any](key string, entry *cacheEntry[V]) int64 {
	size := int64(cacheEntryMemoryOverhead) + int64(len(key))*2
	if encoded, err := json.Marshal(entry.value); err == nil {
		size += int64(len(encoded)) * 2
	}
	if encoded, err := json.Marshal(entry.err); entry.err != nil && err == nil {
		size += int64(len(encoded)) * 2
	}
	return size
}
