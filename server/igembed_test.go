package main

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func wrapEmbed(inner string) string {
	b, _ := json.Marshal(inner)
	return `<script>requireLazy(["a"],function(){__d("PolarisEmbedSimple","init",[],[{"isRichEmbed":true,"contextJSON":` + string(b) + `,"tail":1}])})</script>`
}

func TestParseEmbedPost(t *testing.T) {
	inner := `{"context":{"shortcode":"ABC"},"gql_data":{"shortcode_media":{` +
		`"__typename":"GraphSidecar","id":"1","shortcode":"ABC","is_video":false,` +
		`"owner":{"id":"99","username":"nasa","profile_pic_url":"https://cdn/p.jpg"},` +
		`"edge_media_to_caption":{"edges":[{"node":{"text":"hi there"}}]},` +
		`"edge_liked_by":{"count":10},"edge_media_to_comment":{"count":2},` +
		`"edge_sidecar_to_children":{"edges":[` +
		`{"node":{"id":"c1","is_video":false,"display_url":"https://cdn/1.jpg?oe=1","dimensions":{"width":1080,"height":1350}}},` +
		`{"node":{"id":"c2","is_video":true,"video_url":"https://cdn/2.mp4","display_url":"https://cdn/2.jpg","dimensions":{"width":720,"height":720}}}` +
		`]}}}}`
	post, err := parseEmbedPost(wrapEmbed(inner))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Username != "nasa" || post.Shortcode != "ABC" || post.Caption != "hi there" {
		t.Fatalf("bad post: %+v", post)
	}
	if len(post.Attachments) != 2 {
		t.Fatalf("want 2 attachments, got %d", len(post.Attachments))
	}
	if post.Attachments[0].Kind != "image" || post.Attachments[1].Kind != "video" {
		t.Fatalf("bad kinds: %+v", post.Attachments)
	}
	if post.Attachments[1].URL != "https://cdn/2.mp4" {
		t.Fatalf("video url not used: %q", post.Attachments[1].URL)
	}
}

func TestParseEmbedPostVideoBlocked(t *testing.T) {

	inner := `{"gql_data":{"shortcode_media":{"shortcode":"V","is_video":true,` +
		`"owner":{"id":"1","username":"u"},"display_url":"https://cdn/t.jpg","dimensions":{"width":1,"height":1}}}}`
	if _, err := parseEmbedPost(wrapEmbed(inner)); err == nil || !isTransient(err.Code) {
		t.Fatalf("want transient blocked-video error, got %v", err)
	}
}

func TestParseEmbedCarouselChildWithoutImageFails(t *testing.T) {
	inner := `{"gql_data":{"shortcode_media":{"shortcode":"C","owner":{"username":"u"},"edge_sidecar_to_children":{"edges":[` +
		`{"node":{"id":"c1","display_url":"https://cdn/1.jpg"}},{"node":{"id":"c2"}},{"node":{"id":"c3","display_url":"https://cdn/3.jpg"}}]}}}}`
	if post, err := parseEmbedPost(wrapEmbed(inner)); err == nil || err.Code != errorCodeUpstream {
		t.Fatalf("want upstream error instead of shifted indices, got %+v, %v", post.Attachments, err)
	}
}

func TestParseEmbedSimpleImage(t *testing.T) {
	post, err := parseEmbedPost(simpleEmbedImagePage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Username != "instagram" || post.Shortcode != "DaD8phTyclR" {
		t.Fatalf("bad post: %+v", post)
	}
	if post.ProfilePic != "https://cdn/avatar.jpg?oe=1" {
		t.Fatalf("bad avatar: %q", post.ProfilePic)
	}
	if len(post.Attachments) != 1 || post.Attachments[0].URL != "https://cdn/big.jpg?oe=1" {
		t.Fatalf("bad attachment: %+v", post.Attachments)
	}
	if post.Attachments[0].Width != 3072 || post.Attachments[0].Height != 4095 {
		t.Fatalf("bad dims: %+v", post.Attachments[0])
	}
	if post.Caption != "Hello & #world\nline2" {
		t.Fatalf("bad caption: %q", post.Caption)
	}
	if post.StatsLine != "❤️ 4,809  💬 19" {
		t.Fatalf("bad stats: %q", post.StatsLine)
	}
}

func TestParseEmbedSimpleCollabAvatar(t *testing.T) {
	page := `<script>["PolarisEmbedSimple","init",[],[{"isRichEmbed":false,"contextJSON":null}]]</script>` +
		`<div class="Embed" data-media-type="GraphImage" data-media-id="1" data-owner-id="2" data-permalink="https://www.instagram.com/p/COLLAB/?x">` +
		`<div class="CollabAvatarContainer"><a class="CollabAvatar" href="https://www.instagram.com/collab/?x"><img src="https://cdn/a1.jpg?oe=1" alt="owner" /></a>` +
		`<a class="CollabAvatar SecondCollabAvatar" href="https://www.instagram.com/owner/?x"><img src="https://cdn/a2.jpg?oe=1" alt="owner" /></a></div>` +
		`<span class="UsernameText">owner</span>` +
		`<img class="EmbeddedMediaImage" alt="pic" src="https://cdn/m.jpg?oe=1" />`
	post, err := parseEmbedPost(page)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Username != "owner" || post.ProfilePic != "https://cdn/a2.jpg?oe=1" {
		t.Fatalf("bad collab avatar: %+v", post)
	}
}

func TestParseEmbedSimpleVideoBailsToGraphQL(t *testing.T) {
	page := `<div class="Embed" data-media-type="GraphVideo" data-owner-id="1"><span class="UsernameText">u</span></div>`
	if _, err := parseEmbedPost(page); err == nil || !isTransient(err.Code) {
		t.Fatalf("want transient error so caller falls back to GraphQL, got %v", err)
	}
}

func TestParseEmbedSimpleBrokenMediaBailsToGraphQL(t *testing.T) {
	page := `<script>["PolarisEmbedSimple","init",[],[{"isRichEmbed":false,"contextJSON":null}]]</script>` +
		`<div class="EmbedBrokenMedia"><div class="ebmMessage">The link to this photo or video may be broken, or the post may have been removed.</div></div>`
	if _, err := parseEmbedPost(page); err == nil || !isTransient(err.Code) {
		t.Fatalf("want transient error for broken media, got %v", err)
	}
}

func TestParseEmbedProfile(t *testing.T) {
	inner := `{"context":{"username":"nasa","full_name":"NASA","owner_id":"99",` +
		`"profile_pic_url":"https://cdn/p.jpg","followers_count":104389387,"posts_count":4833,` +
		`"graphql_media":[` +
		`{"shortcode_media":{"id":"m1","shortcode":"S1","display_url":"https://cdn/m1.jpg","dimensions":{"width":100,"height":100},"taken_at_timestamp":1790870385}},` +
		`{"shortcode_media":{"id":"m2","shortcode":"S2","display_url":"https://cdn/m2.jpg","dimensions":{"width":100,"height":100}}}` +
		`]}}`
	p, err := parseEmbedProfile(wrapEmbed(inner))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Username != "nasa" || p.FullName != "NASA" || p.FollowerCount != 104389387 || p.MediaCount != 4833 {
		t.Fatalf("bad profile: %+v", p)
	}
	if len(p.RecentMedia) != 2 || p.RecentMedia[0].Thumbnail != "https://cdn/m1.jpg" {
		t.Fatalf("bad recent media: %+v", p.RecentMedia)
	}
}

// Logged-out GraphQL omits play counts; the rich embed has them.
func TestParseEmbedPostVideoPlayCount(t *testing.T) {
	inner := `{"context":{"shortcode":"V"},"gql_data":{"shortcode_media":{"__typename":"GraphVideo","id":"1","shortcode":"V","is_video":true,` +
		`"video_url":"https://cdn/v.mp4","display_url":"https://cdn/v.jpg","dimensions":{"width":720,"height":1280},"video_view_count":22850111,` +
		`"owner":{"id":"1","username":"u"},"edge_liked_by":{"count":489864},"edge_media_to_comment":{"count":10994}}}}`
	post, err := parseEmbedPost(wrapEmbed(inner))
	if err != nil {
		t.Fatal(err)
	}
	if want := "▶️ 22,850,111  ❤️ 489,864  💬 10,994"; post.StatsLine != want {
		t.Fatalf("StatsLine = %q, want %q", post.StatsLine, want)
	}
}

// The ruling borrows a GraphQL session: a refusal is neither "missing" nor a
// reason to rotate that session.
func TestMediaMissingTrustsOnlyRulingNotFound(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		missing bool
	}{
		{http.StatusUnauthorized, `{"status":"fail"}`, false},
		{http.StatusNotFound, `{"status":"fail","message":"Media cannot be found"}`, true},
		{http.StatusOK, `{"status":"ok"}`, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaTestResponse(r, tc.status, http.Header{"Content-Type": {"application/json"}}, []byte(tc.body)), nil
			})}
			s := &Session{client: client}
			a := &App{pool: &SessionPool{sessions: []*Session{s}}}
			if got, answered := a.mediaMissing(context.Background(), "ABC123"); got != tc.missing || answered != (tc.status != http.StatusUnauthorized) {
				t.Fatalf("mediaMissing = %t, %t; want %t", got, answered, tc.missing)
			}
			if s.client != client || !s.cooldownUntil.IsZero() || s.pending != 0 || s.rttAt.IsZero() != (tc.status == http.StatusUnauthorized) {
				t.Fatalf("session: rotated=%t cooldown=%s pending=%d rttAt=%s", s.client != client, s.cooldownUntil, s.pending, s.rttAt)
			}
		})
	}
}

const ogTestCover = "https://scontent-iad3-1.cdninstagram.com/v/t51.82787-15/1_n.jpg?stp=c418.0.1254.1254a_dst-jpg_e35_s640x640_tt6&_nc_cat=1&oh=00_A&oe=7A000000"

// ogTestPage is a crawler post page as Instagram escapes it; the body JSON
// shows the gate that hides the media.
func ogTestPage(shortcode string, tags ...string) string {
	if tags == nil {
		tags = []string{
			`<meta property="og:title" content="Meta Glasses on Instagram: &quot;Nova. Sage.&quot;" />`,
			`<meta name="twitter:title" content="Meta Glasses (&#064;metaglasses) &#x2022; Instagram photos and videos" />`,
			`<meta property="og:description" content="14K likes, 155 comments - metaglasses on October 15, 2026: &quot;Nova. Sage.&quot;. " />`,
			`<meta property="og:image" content="` + html.EscapeString(ogTestCover) + `" />`,
			`<meta property="og:url" content="https://www.instagram.com/metaglasses/p/` + shortcode + `/" />`,
		}
	}
	return `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8" /><meta property="og:type" content="article" />` +
		strings.Join(tags, "") + `</head><body><script type="application/json">{"gating_type":3,"if_not_gated_logged_out":null}</script></body></html>`
}

// An account minimum-age gate leaves only an error page.
const ogAccountGatedPage = `<!DOCTYPE html><html><head><meta charset="utf-8" /><title>Instagram</title></head><body><script>{"failure_reason":"MA"}</script></body></html>`

func TestParseOGPost(t *testing.T) {
	const code = "Dehfw_5mOy8"
	post, err := parseOGPost(code, ogTestPage(code))
	if err != nil {
		t.Fatal(err)
	}
	want := Post{Shortcode: code, Username: "metaglasses", FullName: "Meta Glasses", Caption: "Nova. Sage.", StatsLine: "❤️ 14K  💬 155",
		Attachments: []Attachment{{Kind: "image", URL: ogTestCover, Thumbnail: ogTestCover}},
		CreatedAt:   shortcodeTime(code), Partial: true}
	if !reflect.DeepEqual(post, want) {
		t.Fatalf("parsed %+v", post)
	}
	if _, ok := cloneAndValidateCacheValue(code, post); !ok || post.CreatedAt.IsZero() {
		t.Fatal("partial post must be a valid cached post with a date")
	}
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	card := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false)
	for _, part := range []string{`content="Meta Glasses (@metaglasses)"`, `❤️ **14K**  💬 **155**`, `/default-avatar.jpg`} {
		if !strings.Contains(card, part) {
			t.Errorf("card lacks %s: %s", part, card)
		}
	}
	// s640x640 bounds the cover; reels and portrait posts come back 360x640 or 512x640.
	if strings.Contains(card, "og:image:width") {
		t.Errorf("card claims a cover size: %s", card)
	}

	if _, err := parseOGPost(code, ogAccountGatedPage); err == nil {
		t.Fatal("page without og tags made a card")
	}
}

func TestParseOGPostRejectsUntrustedTags(t *testing.T) {
	const code = "ABC123"
	tag := func(key, content string) string {
		return `<meta property="` + key + `" content="` + html.EscapeString(content) + `" />`
	}
	image, postURL := tag("og:image", ogTestCover), tag("og:url", "https://www.instagram.com/alice/p/ABC123/")
	for _, tc := range []struct {
		name string
		tags []string
		want string // username, or "" for no card
	}{
		{"no image", []string{postURL}, ""},
		{"foreign image", []string{tag("og:image", "https://evil.example/x.jpg"), postURL}, ""},
		{"plain HTTP image", []string{tag("og:image", "http://scontent.cdninstagram.com/x.jpg"), postURL}, ""},
		{"lookalike image host", []string{tag("og:image", "https://scontent.cdninstagram.com.evil.example/x.jpg"), postURL}, ""},
		{"script image", []string{tag("og:image", "javascript:alert(1)"), postURL}, ""},
		{"no url", []string{image}, ""},
		{"other post", []string{image, tag("og:url", "https://www.instagram.com/alice/p/OTHER1/")}, ""},
		{"foreign url", []string{image, tag("og:url", "https://evil.example/alice/p/ABC123/")}, ""},
		{"bad username", []string{image, tag("og:url", "https://www.instagram.com/a%20b/p/ABC123/")}, ""},
		{"no username", []string{image, tag("og:url", "https://www.instagram.com/p/ABC123/")}, ""},
		{"handle only in title", []string{image, tag("og:url", "https://www.instagram.com/p/ABC123/"), tag("twitter:title", "Bob (@bob) • Instagram")}, ""},
		{"first tag wins", []string{image, postURL, tag("og:image", "https://scontent.cdninstagram.com/later.jpg"), tag("og:url", "https://www.instagram.com/eve/p/ABC123/")}, "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			post, err := parseOGPost(code, ogTestPage(code, tc.tags...))
			if (err == nil) != (tc.want != "") || post.Username != tc.want || err == nil && post.Attachments[0].URL != ogTestCover {
				t.Fatalf("parseOGPost = %+v, %v; want author %q", post, err, tc.want)
			}
		})
	}

	// Markup in a caption stays text, including a raw ">" inside the attribute.
	post, err := parseOGPost(code, ogTestPage(code, image, postURL,
		`<meta property="og:title" content="Eve on Instagram: &quot;&lt;script&gt;alert(1)&lt;/script&gt; a > b&quot;" />`,
		`<meta property="og:description" content="1,234 likes, 1 comment - alice on May 30, 2025: &quot;short&quot;. " />`,
		tag("twitter:title", "Someone else (@eve) • Instagram")))
	if err != nil || post.Caption != "<script>alert(1)</script> a > b" || post.StatsLine != "❤️ 1,234  💬 1" || post.FullName != "" {
		t.Fatalf("parseOGPost = %+v, %v", post, err)
	}
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	if card := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false); strings.Contains(card, "<script>alert") {
		t.Fatalf("caption markup reached the card: %s", card)
	}

	// The display name is free text and may name another handle.
	post, err = parseOGPost(code, ogTestPage(code, image, tag("og:url", "https://www.instagram.com/bob/p/ABC123/"),
		tag("twitter:title", "Bob (@alice) (@bob) • Instagram")))
	if err != nil || post.Username != "bob" || post.FullName != "Bob (@alice)" {
		t.Fatalf("parseOGPost = %+v, %v", post, err)
	}

	// No counts, no caption.
	post, err = parseOGPost(code, ogTestPage(code, image, postURL, tag("og:description", "alice on May 30, 2025")))
	if err != nil || post.StatsLine != "" || post.Caption != "" {
		t.Fatalf("parseOGPost = %+v, %v", post, err)
	}
}
