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

func mediaSelection(values url.Values, pathIndex int) (index int, specified bool) {
	if pathIndex >= 0 {
		return boundedMediaIndex(pathIndex - 1), true
	}
	for _, q := range []struct {
		key  string
		base int
	}{{"img_index", 1}, {"index", 0}, {"order", 0}} {
		if values.Has(q.key) {
			// Present but empty or non-canonical selects 0.
			n, _ := parseCanonicalDecimal(values.Get(q.key))
			return boundedMediaIndex(n - q.base), true
		}
	}
	return 0, false
}

func boundedMediaIndex(index int) int {
	return min(maxCachedMediaItems-1, max(0, index))
}
