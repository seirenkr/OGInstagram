package main

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

const (
	maxSnowcodeDigits     = 256
	maxSnowcodeMediaIndex = 100
)

type snowcodePost struct {
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

func parseStatusSnowcode(code string) snowcodePost {
	if data, ok := decodeSnowcode(code); ok {
		if id, isStr := data["si"].(string); isStr && id != "" {
			su, _ := data["su"].(string)
			g, _ := data["g"].(float64)
			if validUsername(su) && validStoryID(id) {
				return snowcodePost{Username: su, Shortcode: id, Story: true, Gallery: g == 1}
			}
			return snowcodePost{}
		}
		if u, isStr := data["u"].(string); isStr && u != "" {
			if validUsername(u) {
				return snowcodePost{Username: u}
			}
			return snowcodePost{}
		}
		if i, isStr := data["i"].(string); isStr && i != "" {
			if !validShortcode(i) {
				return snowcodePost{}
			}
			p := snowcodePost{Shortcode: i, PostType: "p"}
			if pt, isStr := data["p"].(string); isStr {
				if !isPostRouteType(pt) {
					return snowcodePost{}
				}
				p.PostType = normalizePostType(pt)
			}
			if n, isNum := data["n"].(float64); isNum {
				if math.Trunc(n) != n || n < 1 || n > maxSnowcodeMediaIndex {
					return snowcodePost{}
				}
				idx := int(n) - 1
				p.MediaIndex = idx
				p.Specified = true
			}
			g, _ := data["g"].(float64)
			p.Gallery = g == 1
			return p
		}
	}
	return snowcodePost{Shortcode: code, PostType: "p"}
}

func encodeSnowcodePayload(payload string) string {
	return new(big.Int).SetBytes([]byte(payload)).String()
}

func decodeSnowcode(code string) (map[string]any, bool) {
	if len(code) > maxSnowcodeDigits || (len(code) > 1 && code[0] == '0') ||
		strings.Trim(code, "0123456789") != "" {
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
