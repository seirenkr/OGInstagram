package main

import (
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CJK punctuation ends a URL: captions run the next sentence on with no space.
const urlChars = `[^\s\p{Z}\p{Cc}\p{Cf}<>。、，！？「」『』【】（）]`

var (
	schemeLinkRE = regexp.MustCompile(`(?i)\bhttps?://` + urlChars + `+|\bmailto:` + urlChars + `+`)

	// A mention may not end in a dot; a hashtag needs a letter or digit.
	mentionRE = regexp.MustCompile(`@[A-Za-z0-9._]*[A-Za-z0-9_]`)
	hashtagRE = regexp.MustCompile(`#[\p{L}\p{M}\p{N}_]*[\p{L}\p{N}][\p{L}\p{M}\p{N}_]*`)
)

type linkSpan struct {
	start, end int
	href       string
}

func captionHTML(text string) string {
	spans := detectLinks(text)
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(brHTML(html.EscapeString(text[last:s.start])))
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(s.href))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(text[s.start:s.end]))
		b.WriteString(`</a>`)
		last = s.end
	}
	b.WriteString(brHTML(html.EscapeString(text[last:])))
	return b.String()
}

func brHTML(escaped string) string { return strings.ReplaceAll(escaped, "\n", "<br>") }

func detectLinks(text string) []linkSpan {
	var spans []linkSpan
	occupied := make([]bool, len(text))
	add := func(start, end int, href string) {
		if start < 0 || end > len(text) || start >= end {
			return
		}
		for i := start; i < end; i++ {
			if occupied[i] {
				return
			}
		}
		for i := start; i < end; i++ {
			occupied[i] = true
		}
		spans = append(spans, linkSpan{start, end, href})
	}

	for _, m := range schemeLinkRE.FindAllStringIndex(text, -1) {
		s, e := m[0], trimURLEnd(text, m[0], m[1])
		add(s, e, text[s:e])
	}
	for _, sp := range scanSigil(text, mentionRE, func(handle string) string { return instagramOrigin + "/" + handle }) {
		if h := text[sp.start+1 : sp.end]; validUsername(h) && !strings.Contains(h, "..") {
			add(sp.start, sp.end, sp.href)
		}
	}
	for _, sp := range scanSigil(text, hashtagRE, func(tag string) string {
		return instagramOrigin + "/explore/search/keyword/?q=%23" + url.QueryEscape(tag)
	}) {
		add(sp.start, sp.end, sp.href)
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans
}

var closingToOpening = map[rune]rune{')': '(', ']': '[', '}': '{'}

func trimURLEnd(text string, start, end int) int {
	const trail = ".,;:!?'\"”’»…"
	var count [256]int
	for i := start; i < end; i++ {
		count[text[i]]++
	}
	for end > start {
		r, size := utf8.DecodeLastRuneInString(text[start:end])
		switch {
		case strings.ContainsRune(trail, r):
			end -= size
		case r == ')' || r == ']' || r == '}':
			if count[closingToOpening[r]] >= count[r] {
				return end
			}
			count[r]--
			end -= size
		default:
			return end
		}
	}
	return end
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.M, r)
}

func boundaryOK(text string, start int) bool {
	if start == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:start])
	return !isWordRune(r) && r != '@' && r != '#'
}

func scanSigil(text string, re *regexp.Regexp, href func(name string) string) []linkSpan {
	var out []linkSpan
	lastEnd := -1
	for _, m := range re.FindAllStringIndex(text, -1) {
		if m[0] == lastEnd || boundaryOK(text, m[0]) {
			out = append(out, linkSpan{m[0], m[1], href(text[m[0]+1 : m[1]])})
			lastEnd = m[1]
		}
	}
	return out
}
