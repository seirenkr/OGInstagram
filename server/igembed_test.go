package main

import (
	"encoding/json"
	"testing"
)

// wrapEmbed encodes inner JSON exactly the way IG embeds it: as an escaped
// "contextJSON" string inside the page's JS bootstrap array.
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
	// Single video node with no video_url must fail so the caller uses GraphQL.
	inner := `{"gql_data":{"shortcode_media":{"shortcode":"V","is_video":true,` +
		`"owner":{"id":"1","username":"u"},"display_url":"https://cdn/t.jpg","dimensions":{"width":1,"height":1}}}}`
	if _, err := parseEmbedPost(wrapEmbed(inner)); err == nil {
		t.Fatal("expected blocked-video error, got nil")
	}
}

const simpleEmbedImagePage = `<script>["PolarisEmbedSimple","init",[],[{"isRichEmbed":false,"contextJSON":null}]]</script>` +
	`<div class="Embed" data-media-type="GraphImage" data-media-id="3928250036051888465" data-owner-id="25025320" data-permalink="https://www.instagram.com/p/DaD8phTyclR/?utm_source=ig_embed">` +
	`<a class="Avatar InsideRing" href="https://www.instagram.com/instagram/?utm_source=ig_embed"><img src="https://cdn/avatar.jpg?oe=1" alt="instagram" /></a>` +
	`<span class="UsernameText">instagram</span>` +
	`<div class="Content EmbedFrame" style="padding-bottom: 133.33%;">` +
	`<img class="EmbeddedMediaImage" alt="x" src="https://cdn/small.jpg?oe=1" srcset="https://cdn/big.jpg?oe=1 3072w,https://cdn/small.jpg?oe=1 640w" /></div>` +
	`<div class="SocialProof"><a href="/x">4,809 likes</a></div>` +
	`<div class="Caption"><a class="CaptionUsername" href="/x">instagram</a><br /><br />Hello &amp; <a href="/explore/tags/x">#world</a><br />line2` +
	`<div class="CaptionComments"><a href="/x">View all 19 comments</a></div></div>`

func TestParseEmbedSimpleImage(t *testing.T) {
	post, err := parseEmbedPost(simpleEmbedImagePage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if post.Username != "instagram" || post.Shortcode != "DaD8phTyclR" || post.OwnerID != "25025320" {
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
	if post.Attachments[0].ID != "3928250036051888465" {
		t.Fatalf("bad media id: %q", post.Attachments[0].ID)
	}
	if post.Caption != "Hello & #world\nline2" {
		t.Fatalf("bad caption: %q", post.Caption)
	}
	if post.StatsLine != "❤️ 4,809  💬 19" {
		t.Fatalf("bad stats: %q", post.StatsLine)
	}
}

// Collab posts stack two avatars; the collaborator's plain CollabAvatar comes
// first, the owner's carries SecondCollabAvatar. Expect a2, not a1.
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
	if _, err := parseEmbedPost(page); err == nil {
		t.Fatal("expected error so caller falls back to GraphQL")
	}
}

func TestParseEmbedSimpleBrokenMediaBailsToGraphQL(t *testing.T) {
	page := `<script>["PolarisEmbedSimple","init",[],[{"isRichEmbed":false,"contextJSON":null}]]</script>` +
		`<div class="EmbedBrokenMedia"><div class="ebmMessage">The link to this photo or video may be broken, or the post may have been removed.</div></div>`
	if _, err := parseEmbedPost(page); err == nil {
		t.Fatal("expected error for broken media, got nil")
	}
}

func TestParseEmbedProfile(t *testing.T) {
	inner := `{"context":{"username":"nasa","full_name":"NASA","owner_id":"99",` +
		`"profile_pic_url":"https://cdn/p.jpg","followers_count":104389387,"posts_count":4833,` +
		`"graphql_media":[` +
		`{"shortcode_media":{"id":"m1","shortcode":"S1","display_url":"https://cdn/m1.jpg","dimensions":{"width":100,"height":100}}},` +
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
