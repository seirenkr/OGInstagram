package main

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
)

func TestCachePolicyUsesSuccessfulSource(t *testing.T) {
	profileEmbed := wrapEmbed(`{"context":{"username":"instagram","profile_pic_url":"https://scontent.cdninstagram.com/avatar.jpg"}}`)
	for _, tc := range []struct {
		name    string
		f       *fakeIG
		persist bool
	}{
		{"post_graphql", &fakeIG{gql: gqlFull}, true},
		// A thumbnail stand-in, never persisted.
		{"post_oembed", &fakeIG{gql: `{"data":{}}`, embed: igAnswer{200, "invalid embed"}, oembed: igAnswer{200, oembedPublic}}, false},
		{"profile_embed", &fakeIG{api: igAnswer{200, `{"data":{}}`}, embed: igAnswer{200, profileEmbed}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHelper(t, func(context.Context, string) (Post, *AppError) {
				return Post{}, igErr(http.StatusBadGateway, errorCodeUpstream, helperNoPost)
			})
			synctest.Test(t, func(t *testing.T) {
				tc.f.t = t
				store := newTestStore(t)
				a := newApp(Config{Store: store}, tc.f.pool(), offloadSigner{})
				var err *AppError
				if tc.name == "profile_embed" {
					_, err = a.getProfile(context.Background(), "instagram", nil)
				} else {
					_, err = a.getPost(context.Background(), "ABC123", nil)
				}
				synctest.Wait()
				var writes int
				if qerr := store.state.QueryRow("SELECT COUNT(*) FROM models").Scan(&writes); err != nil || qerr != nil || (writes == 1) != tc.persist {
					t.Fatalf("get = %v; model writes = %d, %v; want persisted %t", err, writes, qerr, tc.persist)
				}
			})
		})
	}
}
