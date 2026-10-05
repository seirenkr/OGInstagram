package main

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

func TestSimpleCommentCountMatchesPreviousParser(t *testing.T) {
	previous := regexp.MustCompile(`([\d,]+)\s+comments`)
	check := func(page string) {
		t.Helper()
		want := parseCount(firstGroup(previous, page))
		if got := simpleCommentCount(page); got != want {
			t.Fatalf("input=%q: got %d, want %d", page, got, want)
		}
	}
	for _, page := range []string{
		"", simpleEmbedImagePage, "1 comment", "View all 1,234 comments",
		"2 commentsSuffix", "comments 99 comments", "12comments 34 comments",
		"12 \t\r\n\fcomments", "12\vcomments", "12\u00a0comments",
		"１２ comments", "12\xff comments", "abc123 comments",
		",,, comments 99 comments", "1,,2 comments", "0 comments 99 comments",
		strings.Repeat("9", 100) + " comments 99 comments",
	} {
		check(page)
	}
	rng := rand.New(rand.NewPCG(123, 456))
	pieces := []string{"x", "comments", " ", "\n", "\t", "\r", "\f", "\v", "\u00a0", "12", ",", "0", "99999999999999999999999999", "<", ">", "안녕", "１２", "\xff"}
	for range 10000 {
		var page strings.Builder
		for range rng.IntN(80) {
			page.WriteString(pieces[rng.IntN(len(pieces))])
		}
		check(page.String())
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
