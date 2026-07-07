package main

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var botRE = regexp.MustCompile(`(?i)bot|discordbot|telegrambot|facebook|twitterbot|slackbot|whatsapp|embed|got|firefox/92|curl|wget|go-http|yahoo|generator|revoltchat|preview|link|proxy|vkshare|images|analyzer|index|crawl|spider|python|node|deno|mastodon|http\.rb|ruby|bun/|fiddler|iframely|bluesky|matrix|cardyb|resolver|feedly|rss|reader|atom|thunderbird|axios`)

type EmbedRoute struct {
	PostType  string
	Shortcode string
	PathIndex int // -1 when the path carries no media index
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

// optionalPathIndex returns the numeric segment at index (-1 when absent);
// ok is false when the segment is present but not a number.
func optionalPathIndex(segments []string, index int) (int, bool) {
	if len(segments) <= index {
		return -1, true
	}
	n, err := strconv.Atoi(segments[index])
	if err != nil {
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

type HomeLocale string

const (
	localeEN     HomeLocale = "en"
	localeJA     HomeLocale = "ja"
	localeKO     HomeLocale = "ko"
	localeZHHant HomeLocale = "zh-hant"
	localeZHHans HomeLocale = "zh-hans"
	localeES     HomeLocale = "es"
	localePT     HomeLocale = "pt"
	localeFR     HomeLocale = "fr"
)

// homeLocales fixes the hreflang emission order: BCP 47 code, alphabetical.
var homeLocales = []HomeLocale{localeEN, localeES, localeFR, localeJA, localeKO, localePT, localeZHHans, localeZHHant}

func asHomeLocale(value string) (HomeLocale, bool) {
	l := HomeLocale(value)
	return l, slices.Contains(homeLocales, l)
}

// matchLocale maps one lowercased Accept-Language tag to a supported locale.
// Chinese needs the script: an explicit Hant script or a TW/HK/MO region picks
// Traditional; any other zh falls back to Simplified.
func matchLocale(tag string) (HomeLocale, bool) {
	if tag == "zh" || strings.HasPrefix(tag, "zh-") {
		if strings.Contains(tag, "hant") || strings.HasSuffix(tag, "-tw") || strings.HasSuffix(tag, "-hk") || strings.HasSuffix(tag, "-mo") {
			return localeZHHant, true
		}
		return localeZHHans, true
	}
	return asHomeLocale(strings.SplitN(tag, "-", 2)[0])
}

func resolveHomeLocale(acceptLanguage string) HomeLocale {
	for _, part := range strings.Split(acceptLanguage, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if loc, ok := matchLocale(tag); ok {
			return loc
		}
	}
	return localeEN
}
