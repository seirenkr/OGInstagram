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

type ShareRoute struct {
	Kind      string
	Token     string
	PathIndex int
}

var shortcodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,24}$`)

var reservedInstagramPaths = map[string]bool{
	"about": true, "accounts": true, "api": true, "challenge": true,
	"channels": true, "direct": true, "download": true, "explore": true,
	"graphql": true, "help": true, "login": true, "notifications": true,
	"p": true, "popular": true, "privacy": true, "profiles": true,
	"qr": true, "reels": true, "session": true, "signup": true,
	"stories": true, "support": true, "terms": true, "users": true,
	"web": true, "your_activity": true,
}

func validShortcode(s string) bool { return shortcodeRE.MatchString(s) }

func isProfileUsername(s string) bool {
	return validUsername(s) && !reservedInstagramPaths[strings.ToLower(s)]
}

func normalizePostType(value string) string {
	if value == "reel" || value == "reels" {
		return "reel"
	}
	return "p"
}

func isPostRouteType(value string) bool {
	return value == "p" || value == "reel" || value == "reels"
}

func parseEmbedSegments(segments []string) *EmbedRoute {
	if len(segments) == 0 {
		return nil
	}
	for i, s := range [][]string{segments, segments[1:]} {
		if i == 1 && (segments[0] == "share" || !validUsername(segments[0])) {
			continue
		}
		if len(s) < 2 || len(s) > 3 || !isPostRouteType(s[0]) || !validShortcode(s[1]) {
			continue
		}
		if i == 0 && (s[0] == "reel" || s[0] == "reels") && s[1] == "audio" {
			return nil
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

func parseStorySegments(segments []string) (username, id string, ok bool) {
	if len(segments) == 3 && segments[0] == "stories" && segments[1] != "highlights" && validUsername(segments[1]) && validStoryID(segments[2]) {
		return segments[1], segments[2], true
	}
	return "", "", false
}

func parseShareSegments(segments []string) *ShareRoute {
	if len(segments) < 2 || segments[0] != "share" {
		return nil
	}
	kind, rest := "", segments[1:]
	if rest[0] == "p" || rest[0] == "reel" {
		kind, rest = rest[0], rest[1:]
	}
	if len(rest) < 1 || len(rest) > 2 || !validShortcode(rest[0]) {
		return nil
	}
	index := -1
	if len(rest) == 2 {
		n, ok := parseCanonicalDecimal(rest[1])
		if !ok {
			return nil
		}
		index = n
	}
	return &ShareRoute{Kind: kind, Token: rest[0], PathIndex: index}
}

func splitPath(path string) []string {
	if trimmed := strings.Trim(path, "/"); trimmed != "" {
		return strings.Split(trimmed, "/")
	}
	return nil
}
