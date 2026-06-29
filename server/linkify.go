package main

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	schemeLinkRE = regexp.MustCompile(`(?i)\b(?:https?|ftps?|file)://[^\s<>]+|\bmailto:[^\s<>]+`)

	emailRE = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}\b`)

	domainRE = regexp.MustCompile(`(?i)\b(?:www\.)?(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}(?::\d+)?(?:[/?#][^\s<>]*)?`)
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
		b.WriteString(brHTML(htmlEscape(text[last:s.start])))
		b.WriteString(`<a href="` + htmlEscape(s.href) + `">`)
		b.WriteString(htmlEscape(text[s.start:s.end]))
		b.WriteString(`</a>`)
		last = s.end
	}
	b.WriteString(brHTML(htmlEscape(text[last:])))
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
		if !hasKnownTLD(text[s:e]) {
			continue
		}
		add(s, e, "https://"+text[s:e])
	}
	for _, sp := range scanMentions(text) {
		add(sp.start, sp.end, sp.href)
	}
	for _, sp := range scanHashtags(text) {
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

func trimURLEnd(text string, start, end int) int {
	const trail = ".,;:!?'\"”’»…"
	for end > start {
		r, size := utf8.DecodeLastRuneInString(text[start:end])
		switch {
		case strings.ContainsRune(trail, r):
			end -= size
		case r == ')' || r == ']' || r == '}':
			open := map[rune]rune{')': '(', ']': '[', '}': '{'}[r]
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

func scanMentions(text string) []linkSpan {
	var out []linkSpan
	lastEnd := -1
	for i := 0; i < len(text); {
		if text[i] != '@' || (!boundaryOK(text, i) && i != lastEnd) {
			i++
			continue
		}
		j := i + 1
		for j < len(text) {
			c := text[j]
			if c == '.' || c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
				j++
				continue
			}
			break
		}
		for j > i+1 && text[j-1] == '.' {
			j--
		}
		if j > i+1 {
			handle := text[i+1 : j]
			out = append(out, linkSpan{i, j, instagramOrigin + "/" + handle})
			i = j
			lastEnd = j
			continue
		}
		i++
	}
	return out
}

func scanHashtags(text string) []linkSpan {
	var out []linkSpan
	lastEnd := -1
	for i := 0; i < len(text); {
		if text[i] != '#' || (!boundaryOK(text, i) && i != lastEnd) {
			i++
			continue
		}
		j := i + 1
		hasAlnum := false
		for j < len(text) {
			r, size := utf8.DecodeRuneInString(text[j:])
			if r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) {
				if unicode.IsLetter(r) || unicode.IsNumber(r) {
					hasAlnum = true
				}
				j += size
				continue
			}
			break
		}
		if j > i+1 && hasAlnum {
			tag := text[i+1 : j]
			out = append(out, linkSpan{i, j, instagramOrigin + "/explore/search/keyword/?q=%23" + url.QueryEscape(tag)})
			i = j
			lastEnd = j
			continue
		}
		i++
	}
	return out
}
