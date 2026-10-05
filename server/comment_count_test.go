package main

import (
	"strings"
	"testing"
)

func TestSimpleCommentCount(t *testing.T) {
	for page, want := range map[string]int{
		"":                        0,
		"1 comment":               0,
		"View all 1,234 comments": 1234,
		"12 \t\r\n\fcomments":     12,
		"12comments 34 comments":  34,
		"comments 99 comments":    99,
		"abc123 comments":         123,
		"0 comments 99 comments":  0,
		"２ comments 5 comments":   5,
	} {
		if got := simpleCommentCount(page); got != want {
			t.Errorf("simpleCommentCount(%q) = %d, want %d", page, got, want)
		}
	}
	post, err := parseEmbedPost(strings.ReplaceAll(simpleEmbedImagePage, "19 comments", "1,234 comments"))
	if err != nil || post.StatsLine != "❤️ 4,809  💬 1,234" {
		t.Fatalf("embed stats=%q, err=%v", post.StatsLine, err)
	}
}

func BenchmarkParseEmbedComments(b *testing.B) {
	const size = 256 << 10
	const snippet = `<script type="application/json" data-sjs>{"bootload":{"id":123456,"size":45678,"url":"https://static.example.invalid/runtime.js"},"comments_count":42,"nodes":[1,2,3,4,5]}</script>`
	pad := strings.Repeat(snippet, size/len(snippet)+1)[:size]
	for _, tc := range []struct{ name, page string }{
		{"front", simpleEmbedImagePage + pad},
		{"middle", pad[:size/2] + simpleEmbedImagePage + pad[size/2:]},
		{"end", pad + simpleEmbedImagePage},
		{"absent", strings.ReplaceAll(simpleEmbedImagePage, "19 comments", "") + pad},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := parseEmbedPost(tc.page); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
