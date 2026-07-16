package main

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

type snowPost struct {
	Username   string
	Shortcode  string
	PostType   string
	MediaIndex int
	Specified  bool
	Gallery    bool
	Story      bool
}

func profileSnowcode(username string) string {
	return encodeSnowcodePayload(`"u":"` + username + `"`)
}

func storyStatusSnowcode(username, id string, gallery bool) string {
	payload := `"su":"` + username + `","si":"` + id + `"`
	if gallery {
		payload += `,"g":1`
	}
	return encodeSnowcodePayload(payload)
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
	return encodeSnowcodePayload(payload)
}

func parseStatusSnowcode(code string) snowPost {
	if data, ok := decodeSnowcode(code); ok {
		if id, isStr := data["si"].(string); isStr && id != "" {
			su, _ := data["su"].(string)
			g, _ := data["g"].(float64)
			return snowPost{Username: su, Shortcode: id, Story: true, Gallery: g == 1}
		}
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

func encodeSnowcodePayload(payload string) string {
	return new(big.Int).SetBytes([]byte(payload)).String()
}

func decodeSnowcode(code string) (map[string]any, bool) {
	if code == "" || strings.Trim(code, "0123456789") != "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(code, 10)
	if !ok {
		return nil, false
	}
	var out map[string]any
	if json.Unmarshal(append(append([]byte{'{'}, n.Bytes()...), '}'), &out) != nil {
		return nil, false
	}
	return out, true
}
