package main

import (
	"strings"
	"testing"
)

func allDigits(s string) bool {
	return s != "" && strings.TrimFunc(s, func(r rune) bool { return r >= '0' && r <= '9' }) == ""
}

func TestStatusSnowcodeRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		postType  string
		shortcode string
		index     int
		specified bool
		gallery   bool
	}{
		{"plain post", "p", "DZ4YS7sktCx", 0, false, false},
		{"reel", "reel", "CabC_d-Efg", 0, false, false},
		{"specific image", "p", "ABC123xyz", 2, true, false},
		{"gallery", "p", "Short_code-1", 0, false, true},
		{"reel specific gallery", "reel", "z9", 4, true, true},
	}
	for _, c := range cases {
		code := statusSnowcode(c.postType, c.shortcode, c.index, c.specified, c.gallery)
		if !allDigits(code) {
			t.Errorf("%s: snowcode not all digits: %q", c.name, code)
		}
		got := parseStatusSnowcode(code)
		if got.Shortcode != c.shortcode {
			t.Errorf("%s: shortcode=%q want %q", c.name, got.Shortcode, c.shortcode)
		}
		if got.PostType != normalizePostType(c.postType) {
			t.Errorf("%s: postType=%q want %q", c.name, got.PostType, normalizePostType(c.postType))
		}
		if got.Specified != c.specified {
			t.Errorf("%s: specified=%v want %v", c.name, got.Specified, c.specified)
		}
		if c.specified && got.MediaIndex != c.index {
			t.Errorf("%s: index=%d want %d", c.name, got.MediaIndex, c.index)
		}
		if got.Gallery != c.gallery {
			t.Errorf("%s: gallery=%v want %v", c.name, got.Gallery, c.gallery)
		}
	}
}

func TestProfileSnowcodeRoundTrip(t *testing.T) {
	for _, username := range []string{"instagram", "k_i_m_i_n_", "a.b.c"} {
		code := profileSnowcode(username)
		if !allDigits(code) {
			t.Errorf("%s: profile snowcode not all digits: %q", username, code)
		}
		got := parseStatusSnowcode(code)
		if got.Username != username || got.Shortcode != "" {
			t.Errorf("%s: parsed=%#v, want Username only", username, got)
		}
	}
}

func TestParseStatusSnowcodeFallsBackToRawShortcode(t *testing.T) {

	got := parseStatusSnowcode("DZ4YS7sktCx")
	if got.Shortcode != "DZ4YS7sktCx" || got.PostType != "p" {
		t.Errorf("raw shortcode fallback failed: %#v", got)
	}
}
