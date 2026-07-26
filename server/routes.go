package main

import (
	"net/url"
	"regexp"
	"strings"
)

type EmbedRoute struct {
	PostType  string
	Shortcode string
	PathIndex int
}

var shortcodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,24}$`)

func validShortcode(s string) bool { return shortcodeRE.MatchString(s) }

func normalizePostType(value string) string {
	if value == "reel" || value == "reels" {
		return "reel"
	}
	return "p"
}

func isPostRouteType(value string) bool {
	return value == "p" || value == "reel" || value == "reels"
}

func optionalPathIndex(segments []string, index int) (int, bool) {
	if len(segments) <= index {
		return -1, true
	}
	n, ok := parseCanonicalDecimal(segments[index])
	if !ok {
		return -1, false
	}
	return n, true
}

func parseEmbedSegments(segments []string) *EmbedRoute {
	if (len(segments) == 2 || len(segments) == 3) && isPostRouteType(segments[0]) && validShortcode(segments[1]) {
		idx, ok := optionalPathIndex(segments, 2)
		if !ok {
			return nil
		}
		return &EmbedRoute{PostType: normalizePostType(segments[0]), Shortcode: segments[1], PathIndex: idx}
	}
	if (len(segments) == 3 || len(segments) == 4) && isPostRouteType(segments[1]) && validShortcode(segments[2]) {
		idx, ok := optionalPathIndex(segments, 3)
		if !ok {
			return nil
		}
		return &EmbedRoute{PostType: normalizePostType(segments[1]), Shortcode: segments[2], PathIndex: idx}
	}
	return nil
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	out := make([]string, len(parts))
	for i, seg := range parts {
		if dec, err := url.PathUnescape(seg); err == nil {
			out[i] = dec
		} else {
			out[i] = seg
		}
	}
	return out
}
