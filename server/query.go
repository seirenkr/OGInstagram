package main

import (
	"net/url"
	"strconv"
)

func parseCanonicalDecimal(raw string) (int, bool) {
	if raw == "" || (raw != "0" && raw[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(raw, 10, 53)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

func queryInt(values url.Values, key string) (int, bool) {
	if _, has := values[key]; !has {
		return 0, false
	}
	raw := values.Get(key)
	if raw == "" {
		return 0, true
	}
	n, ok := parseCanonicalDecimal(raw)
	if !ok {
		return 0, true
	}
	return n, true
}

func mediaSelection(values url.Values, pathIndex int) (index int, specified bool) {
	if pathIndex >= 0 {
		return boundedMediaIndex(pathIndex - 1), true
	}
	if n, ok := queryInt(values, "img_index"); ok {
		return boundedMediaIndex(n - 1), true
	}
	if n, ok := queryInt(values, "index"); ok {
		return boundedMediaIndex(n), true
	}
	if n, ok := queryInt(values, "order"); ok {
		return boundedMediaIndex(n), true
	}
	return 0, false
}

func boundedMediaIndex(index int) int {
	return min(maxCachedMediaItems-1, max(0, index))
}

func galleryRequested(values url.Values) bool { return values.Get("__gallery") == "1" }
