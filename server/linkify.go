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

var (
	schemeLinkRE = regexp.MustCompile(`(?i)\bhttps?://[^\s<>]+|\bmailto:[^\s<>]+`)

	emailRE = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}\b`)

	domainRE = regexp.MustCompile(`(?i)\b(?:www\.)?(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}(?::\d+)?(?:[/?#][^\s<>]*)?`)

	// A mention may not end in a dot; a hashtag needs a letter or digit.
	mentionRE = regexp.MustCompile(`@[A-Za-z0-9._]*[A-Za-z0-9_]`)
	hashtagRE = regexp.MustCompile(`#[\p{L}\p{N}_]*[\p{L}\p{N}][\p{L}\p{N}_]*`)
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
	for _, m := range emailRE.FindAllStringIndex(text, -1) {
		add(m[0], m[1], "mailto:"+text[m[0]:m[1]])
	}
	for _, m := range domainRE.FindAllStringIndex(text, -1) {
		s, e := m[0], trimURLEnd(text, m[0], m[1])
		if s >= 3 && text[s-3:s] == "://" {
			continue
		}
		if !hasKnownTLD(text[s:e]) {
			continue
		}
		add(s, e, "https://"+text[s:e])
	}
	for _, sp := range scanSigil(text, mentionRE, func(handle string) string { return instagramOrigin + "/" + handle }) {
		add(sp.start, sp.end, sp.href)
	}
	for _, sp := range scanSigil(text, hashtagRE, func(tag string) string {
		return instagramOrigin + "/explore/search/keyword/?q=%23" + url.QueryEscape(tag)
	}) {
		add(sp.start, sp.end, sp.href)
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans
}

func hasKnownTLD(candidate string) bool {
	host := candidate
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	_, ok := tldSet[strings.ToLower(labels[len(labels)-1])]
	return ok
}

var closingToOpening = map[rune]rune{')': '(', ']': '[', '}': '{'}

func trimURLEnd(text string, start, end int) int {
	const trail = ".,;:!?'\"”’»…"
	for end > start {
		r, size := utf8.DecodeLastRuneInString(text[start:end])
		switch {
		case strings.ContainsRune(trail, r):
			end -= size
		case r == ')' || r == ']' || r == '}':
			open := closingToOpening[r]
			if strings.Count(text[start:end], string(open)) >= strings.Count(text[start:end], string(r)) {
				return end
			}
			end -= size
		default:
			return end
		}
	}
	return end
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
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
