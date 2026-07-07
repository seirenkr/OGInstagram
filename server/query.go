package main

import (
	"net/url"
	"strconv"
)

func queryInt(values url.Values, key string) (int, bool) {
	if _, has := values[key]; !has {
		return 0, false
	}
	raw := values.Get(key)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, true
	}
	return n, true
}

// mediaSelection resolves the requested carousel item: the path index
// (1-based) wins, then ?img_index (1-based), then ?index / ?order (0-based).
// specified reports whether any of them was present.
func mediaSelection(values url.Values, pathIndex int) (index int, specified bool) {
	if pathIndex >= 0 {
		return max(0, pathIndex-1), true
	}
	if n, ok := queryInt(values, "img_index"); ok {
		return max(0, n-1), true
	}
	if n, ok := queryInt(values, "index"); ok {
		return n, true
	}
	if n, ok := queryInt(values, "order"); ok {
		return n, true
	}
	return 0, false
}

func galleryRequested(values url.Values) bool { return values.Get("__gallery") == "1" }
