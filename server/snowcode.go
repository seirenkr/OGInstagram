package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

func activityCodeFor(postType, shortcode string, mediaIndex int, specified, gallery bool) string {
	normalized := normalizePostType(postType)
	payload := `"i":"` + shortcode + `"`
	if normalized != "p" {
		payload += `,"p":"` + normalized + `"`
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

func profileActivityCode(username string) string {
	if encoded, ok := encodeSnowcodePayload(`"u":"` + username + `"`); ok {
		return encoded
	}
	return username
}

func parseActivityCode(code string) ActivityRoute {
	if data, ok := decodeSnowcode(code); ok {
		if u, isStr := data["u"].(string); isStr && u != "" {
			return ActivityRoute{Username: u}
		}
		if i, isStr := data["i"].(string); isStr && i != "" {
			route := ActivityRoute{Shortcode: i, PostType: "p"}
			if p, isStr := data["p"].(string); isStr {
				route.PostType = normalizePostType(p)
			}
			if n, isNum := data["n"].(float64); isNum {
				idx := int(n) - 1
				if idx < 0 {
					idx = 0
				}
				route.MediaIndex = idx
				route.MediaIndexSpecified = true
			}
			if g, isNum := data["g"].(float64); isNum && g == 1 {
				route.Gallery = true
			}
			return route
		}
	}
	return ActivityRoute{Shortcode: code, PostType: "p"}
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
	if raw == "" || len(raw)%2 != 0 {
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
