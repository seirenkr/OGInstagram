package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestVerificationFromAuthorJSON(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(string) (bool, *AppError)
	}{
		{"post GraphQL user", func(field string) (bool, *AppError) {
			body := strings.Replace(sampleLoggedOut, `"full_name":"Instagram"`, `"full_name":"Instagram"`+field, 1)
			post, err := parseInstagramPost(body)
			return post.IsVerified, err
		}},
		{"post embed owner", func(field string) (bool, *AppError) {
			post, err := parseEmbedPost(wrapEmbed(`{"gql_data":{"shortcode_media":{"shortcode":"ABC","is_video":false,` +
				`"owner":{"username":"nasa","full_name":"NASA ✅"` + field + `},"display_url":"https://cdn/p.jpg"}}}`))
			return post.IsVerified, err
		}},
		{"profile API user", func(field string) (bool, *AppError) {
			profile, err := parseProfile(`{"data":{"user":{"username":"nasa","full_name":"NASA ✅",` +
				`"edge_followed_by":{"count":100000000}` + field + `}}}`)
			return profile.IsVerified, err
		}},
		{"profile embed context", func(field string) (bool, *AppError) {
			profile, err := parseEmbedProfile(wrapEmbed(`{"context":{"username":"instagram","owner_id":25025320,"full_name":"Instagram"` + field + `,"graphql_media":[]}}`))
			return profile.IsVerified, err
		}},
	}
	for _, parser := range parsers {
		t.Run(parser.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, field string
				want        bool
			}{
				{"true", `,"is_verified":true`, true},
				{"false", `,"is_verified":false`, false},
				{"missing", "", false},
				{"null", `,"is_verified":null`, false},
				{"string is not a boolean", `,"is_verified":"true"`, false},
				{"number is not a boolean", `,"is_verified":1`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					verified, err := parser.parse(tc.field)
					if err != nil || verified != tc.want {
						t.Fatalf("verification = %v, %v; want %v", verified, err, tc.want)
					}
				})
			}
		})
	}
}

func TestVerificationNotInferredForFallbackSources(t *testing.T) {
	post, err := parseEmbedPost(simpleEmbedImagePage)
	if err != nil || post.IsVerified {
		t.Fatalf("simple HTML must not infer verification from the Instagram username: %+v, %v", post, err)
	}
	profile, err := parseEmbedProfile(wrapEmbed(`{"context":{"username":"nasa","full_name":"NASA ✅","followers_count":100000000,"graphql_media":[]}}`))
	if err != nil || profile.IsVerified {
		t.Fatalf("profile embed without a verification signal must remain unverified: %+v, %v", profile, err)
	}
	post, ok := parseOembedPost("ABC", `{"author_name":"instagram","title":"Verified ✅","thumbnail_url":"https://i.cdninstagram.com/p.jpg"}`)
	if !ok || post.IsVerified {
		t.Fatalf("oEmbed author names must not imply verification: %+v, %v", post, ok)
	}
}

func TestVerificationSurvivesModelCacheRefresh(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	c := newPersistentCache[Post](store, "post", make(chan struct{}, 1), localPostCacheBytes)
	const key = "ABC"
	legacy := []byte(`{"value":{"Shortcode":"ABC","Username":"nasa","FullName":"NASA","Attachments":[{"Kind":"image","URL":"https://i.cdninstagram.com/p.jpg"}]}}`)
	if err := store.putModel(ctx, "post", key, legacy, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	entry, ok := c.persistentGet(ctx, key)
	if !ok || entry.value.IsVerified {
		t.Fatal("existing cached model without IsVerified must remain usable and unverified")
	}
	refreshed, err := parseEmbedPost(wrapEmbed(`{"gql_data":{"shortcode_media":{"shortcode":"ABC","is_video":false,` +
		`"owner":{"username":"nasa","is_verified":true},"display_url":"https://i.cdninstagram.com/p.jpg"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c.persistentPut(ctx, key, &cacheEntry[Post]{value: refreshed, expiresAt: time.Now().Add(time.Hour)})
	reloaded, ok := c.persistentGet(ctx, key)
	if !ok || !reloaded.value.IsVerified {
		t.Fatal("refreshed verification was lost through model serialization or cache normalization")
	}
}
