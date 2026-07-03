package main

import (
	"strconv"
	"testing"
	"time"
)

func oeURL(t time.Time) string {
	return "https://cdn/x.jpg?oe=" + strconv.FormatInt(t.Unix(), 16) + "&oh=abc"
}

func TestCacheTTLFromURLs(t *testing.T) {
	near := time.Now().Add(3 * time.Hour)
	far := time.Now().Add(100 * time.Hour)
	// earliest expiry (minus margin) wins
	ttl := cacheTTLFromURLs(oeURL(far), oeURL(near))
	if ttl < 2*time.Hour+25*time.Minute || ttl > 2*time.Hour+30*time.Minute {
		t.Errorf("expected ~2h30m (3h - 30m margin), got %v", ttl)
	}
	// no oe -> 24h fallback
	if got := cacheTTLFromURLs("https://cdn/x.jpg", ""); got != cdnFallbackTTL {
		t.Errorf("fallback: got %v want %v", got, cdnFallbackTTL)
	}
	// already expired -> floored at 1m
	if got := cacheTTLFromURLs(oeURL(time.Now().Add(-time.Hour))); got != time.Minute {
		t.Errorf("expired floor: got %v want 1m", got)
	}
}
