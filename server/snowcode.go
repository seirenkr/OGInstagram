package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// A snowcode is an all-digits id encoding the whole post identity, because Discord re-requests /api/v1/statuses/{id} without the query string.
type snowPost struct {
	Username   string
	Shortcode  string
	PostType   string
	MediaIndex int
	Specified  bool
	Gallery    bool
}

func profileSnowcode(username string) string {
	if encoded, ok := encodeSnowcodePayload(`"u":"` + username + `"`); ok {
		return encoded
	}
	return username
}

func statusSnowcode(postType, shortcode string, mediaIndex int, specified, gallery bool) string {
	payload := `"i":"` + shortcode + `"`
	if n := normalizePostType(postType); n != "p" {
		payload += `,"p":"` + n + `"`
	}
	if specified {
		idx := mediaIndex
		if idx < 0 {
			idx = 0
		}
		payload += `,"n":` + strconv.Itoa(idx+1)
	}
	if gallery {
		payload += `,"g":1`
	}
	if encoded, ok := encodeSnowcodePayload(payload); ok {
		return encoded
	}
	return shortcode
}

func parseStatusSnowcode(code string) snowPost {
	if data, ok := decodeSnowcode(code); ok {
		if u, isStr := data["u"].(string); isStr && u != "" {
			return snowPost{Username: u}
		}
		if i, isStr := data["i"].(string); isStr && i != "" {
			p := snowPost{Shortcode: i, PostType: "p"}
			if pt, isStr := data["p"].(string); isStr {
				p.PostType = normalizePostType(pt)
			}
			if n, isNum := data["n"].(float64); isNum {
				idx := int(n) - 1
				if idx < 0 {
					idx = 0
				}
				p.MediaIndex = idx
				p.Specified = true
			}
			if g, isNum := data["g"].(float64); isNum && g == 1 {
				p.Gallery = true
			}
			return p
		}
	}
	return snowPost{Shortcode: code, PostType: "p", MediaIndex: 0}
}

func encodeSnowcodePayload(payload string) (string, bool) {
	var b strings.Builder
	b.Grow(len(payload) * 2)
	for _, ch := range payload {
		idx := strings.IndexRune(snowcodeChars, ch)
		if idx < 0 {
			return "", false
		}
		b.WriteByte('0' + byte(idx/10))
		b.WriteByte('0' + byte(idx%10))
	}
	return b.String(), true
}

func decodeSnowcode(code string) (map[string]any, bool) {
	var digits strings.Builder
	for _, ch := range code {
		if ch >= '0' && ch <= '9' {
			digits.WriteRune(ch)
		}
	}
	raw := digits.String()
	if raw == "" || len(raw)%2 != 0 || len(raw) != len(code) {
		return nil, false
	}
	var payload strings.Builder
	for i := 0; i < len(raw); i += 2 {
		idx, err := strconv.Atoi(raw[i : i+2])
		if err != nil || idx < 0 || idx >= len(snowcodeChars) {
			return nil, false
		}
		payload.WriteByte(snowcodeChars[idx])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte("{"+payload.String()+"}"), &out); err != nil {
		return nil, false
	}
	return out, true
}
