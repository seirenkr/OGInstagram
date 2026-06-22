package main

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var botRE = regexp.MustCompile(`(?i)bot|discordbot|telegrambot|facebook|twitterbot|slackbot|whatsapp|embed|got|firefox/92|curl|wget|go-http|yahoo|generator|revoltchat|preview|link|proxy|vkshare|images|analyzer|index|crawl|spider|python|node|deno|mastodon|http\.rb|ruby|bun/|fiddler|iframely|bluesky|matrix|cardyb|resolver|feedly|rss|reader|atom|thunderbird|axios`)

type EmbedRoute struct {
	PostType  string
	Shortcode string
	PathIndex int
	HasIndex  bool
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

func optionalPathIndex(segments []string, index int) (int, bool, bool) {
	if len(segments) <= index {
		return 0, false, true
	}
	n, err := strconv.Atoi(segments[index])
	if err != nil {
		return 0, false, false
	}
	return n, true, true
}

func parseEmbedSegments(segments []string) *EmbedRoute {
	if (len(segments) == 2 || len(segments) == 3) && isPostRouteType(segments[0]) && validShortcode(segments[1]) {
		idx, has, ok := optionalPathIndex(segments, 2)
		if !ok {
			return nil
		}
		return &EmbedRoute{PostType: normalizePostType(segments[0]), Shortcode: segments[1], PathIndex: idx, HasIndex: has}
	}
	if (len(segments) == 3 || len(segments) == 4) && isPostRouteType(segments[1]) && validShortcode(segments[2]) {
		idx, has, ok := optionalPathIndex(segments, 3)
		if !ok {
			return nil
		}
		return &EmbedRoute{PostType: normalizePostType(segments[1]), Shortcode: segments[2], PathIndex: idx, HasIndex: has}
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

type HomeLocale string

const (
	localeEN HomeLocale = "en"
	localeJA HomeLocale = "ja"
	localeKO HomeLocale = "ko"
)

func asHomeLocale(value string) (HomeLocale, bool) {
	switch value {
	case "en":
		return localeEN, true
	case "ja":
		return localeJA, true
	case "ko":
		return localeKO, true
	}
	return "", false
}

func resolveHomeLocale(acceptLanguage string) HomeLocale {
	for _, part := range strings.Split(acceptLanguage, ",") {
		primary := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		primary = strings.SplitN(primary, "-", 2)[0]
		if loc, ok := asHomeLocale(primary); ok {
			return loc
		}
	}
	return localeEN
}

func homePathLocale(path string) (HomeLocale, bool) {
	return asHomeLocale(strings.Trim(path, "/"))
}
