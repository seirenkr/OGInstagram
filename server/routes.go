package main

import (
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

// parseEmbedSegments accepts /p/X[/n] and /user/p/X[/n]. A shape match with a
// non-canonical index rejects the path instead of trying the other shape.
func parseEmbedSegments(segments []string) *EmbedRoute {
	for _, s := range [][]string{segments, segments[min(1, len(segments)):]} {
		if len(s) < 2 || len(s) > 3 || !isPostRouteType(s[0]) || !validShortcode(s[1]) {
			continue
		}
		index := -1
		if len(s) == 3 {
			n, ok := parseCanonicalDecimal(s[2])
			if !ok {
				return nil
			}
			index = n
		}
		return &EmbedRoute{PostType: normalizePostType(s[0]), Shortcode: s[1], PathIndex: index}
	}
	return nil
}

func splitPath(path string) []string {
	if trimmed := strings.Trim(path, "/"); trimmed != "" {
		return strings.Split(trimmed, "/")
	}
	return nil
}
