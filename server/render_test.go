package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildEmbedHTMLGalleryLeavesDescriptionEmpty(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram", BrandColor: "#ff0069"}}
	post := Post{
		Shortcode: "CODE",
		Username:  "user",
		FullName:  "User",
		Caption:   "caption",
		StatsLine: "stats",
		Attachments: []Attachment{{
			Kind: "image", Width: 1080, Height: 1080,
		}},
	}

	normal := a.buildEmbedHTML("https://oginstagram.com", "Discordbot", post, "p", 0, false, false)
	if !strings.Contains(normal, "property=\"og:description\" content=\"stats\n\ncaption\"") {
		t.Fatalf("normal embed description missing: %s", normal)
	}

	gallery := a.buildEmbedHTML("https://oginstagram.com", "Discordbot", post, "p", 0, false, true)
	for _, tag := range []string{
		`name="description" content=""`,
		`property="og:description" content=""`,
		`name="twitter:description" content=""`,
	} {
		if !strings.Contains(gallery, tag) {
			t.Errorf("gallery embed is missing %s", tag)
		}
	}
	if strings.Contains(gallery, `property="og:image:alt"`) || strings.Contains(gallery, `property="twitter:image:alt"`) {
		t.Error("gallery embed must not expose the caption through image alt metadata")
	}

	wantHref := statusURL("https://oginstagram.com", "user", "p", "CODE", 0, false, true)
	if !strings.Contains(gallery, `href="`+wantHref+`"`) {
		t.Errorf("gallery embed activity link wrong, want %s in: %s", wantHref, gallery)
	}

	var note map[string]any
	body := a.buildActivityStatus("https://oginstagram.com", post, "p", 0, false, true)
	if err := json.Unmarshal(body, &note); err != nil {
		t.Fatal(err)
	}
	ctx, ok := note["@context"].(string)
	if !ok || ctx != "https://www.w3.org/ns/activitystreams" || note["type"] != "Note" {
		t.Errorf("not a standard AS2 Note: @context=%v type=%v", note["@context"], note["type"])
	}
	if note["id"] != wantHref {
		t.Errorf("Note id wrong, got %v want %v", note["id"], wantHref)
	}
	if note["attributedTo"] != "https://oginstagram.com/users/user" {
		t.Errorf("attributedTo must be the actor URL, got %v", note["attributedTo"])
	}
	if note["content"] != "" {
		t.Errorf("gallery Note content = %q, want empty", note["content"])
	}
	att, ok := note["attachment"].([]any)
	if !ok || len(att) != 1 {
		t.Fatalf("gallery Note attachment = %#v, want one item", note["attachment"])
	}
	if m := att[0].(map[string]any); m["type"] != "Document" || m["mediaType"] != "image/jpeg" {
		t.Errorf("attachment should be Document+mediaType: %#v", m)
	}

	for _, k := range []string{"media_attachments", "spoiler_text", "visibility", "account", "sensitive", "uri", "reblog"} {
		if _, present := note[k]; present {
			t.Errorf("non-standard field %q must not appear", k)
		}
	}
}

func TestBuildMastodonStatusVideoHasPreviewURL(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram"}}
	post := Post{
		Shortcode: "CODE", Username: "user", FullName: "User", StatsLine: "stats",
		Attachments: []Attachment{{Kind: "video", URL: "https://cdn/x.mp4", Thumbnail: "https://cdn/x.jpg", Width: 1080, Height: 1080}},
	}
	var st map[string]any
	if err := json.Unmarshal(a.buildMastodonStatus("https://oginstagram.com", post, "reel", 0, false, false), &st); err != nil {
		t.Fatal(err)
	}

	wantID := statusSnowcode("reel", "CODE", 0, false, false)
	if st["id"] != wantID || st["visibility"] != "public" {
		t.Errorf("status core fields wrong: id=%v want=%v visibility=%v", st["id"], wantID, st["visibility"])
	}
	if strings.TrimFunc(wantID, func(r rune) bool { return r >= '0' && r <= '9' }) != "" {
		t.Errorf("snowcode id must be all digits, got %q", wantID)
	}

	for _, k := range []string{"media_attachments", "mentions", "tags", "emojis"} {
		if _, ok := st[k].([]any); !ok {
			t.Errorf("%q must be a present array", k)
		}
	}
	if _, ok := st["account"].(map[string]any); !ok {
		t.Fatal("account object missing")
	}
	media, ok := st["media_attachments"].([]any)
	if !ok || len(media) != 1 {
		t.Fatalf("want one media_attachment, got %#v", st["media_attachments"])
	}
	m := media[0].(map[string]any)
	if m["type"] != "video" {
		t.Errorf("media type = %v, want video", m["type"])
	}

	wantPreview := "https://oginstagram.com/offload/CODE/1?thumbnail=1"
	if m["preview_url"] != wantPreview {
		t.Errorf("preview_url = %v, want %v", m["preview_url"], wantPreview)
	}
	if m["url"] != "https://oginstagram.com/offload/CODE/1" {
		t.Errorf("url = %v, want playable offload", m["url"])
	}
}

func TestBuildMastodonStatusUsesNullsForMissingEmbedFields(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram"}}
	post := Post{
		Shortcode: "CODE",
		Username:  "user",
		StatsLine: "stats",
		Attachments: []Attachment{{
			Kind: "image", URL: "https://cdn/x.jpg", Thumbnail: "https://cdn/x.jpg", Width: 1080, Height: 1080,
		}},
	}

	var st map[string]any
	if err := json.Unmarshal(a.buildMastodonStatus("https://oginstagram.com", post, "p", 0, false, false), &st); err != nil {
		t.Fatal(err)
	}
	if st["created_at"] != nil {
		t.Fatalf("missing post timestamp must be JSON null, got %#v", st["created_at"])
	}
	for _, k := range []string{"edited_at", "poll", "card", "language", "text", "quote"} {
		if st[k] != nil {
			t.Errorf("%s must be JSON null when unavailable, got %#v", k, st[k])
		}
	}
	if st["quotes_count"] != float64(0) {
		t.Errorf("quotes_count = %#v, want 0", st["quotes_count"])
	}
	if _, ok := st["tagged_collections"].([]any); !ok {
		t.Errorf("tagged_collections must be a present array, got %#v", st["tagged_collections"])
	}
	qa, ok := st["quote_approval"].(map[string]any)
	if !ok || qa["current_user"] != "denied" {
		t.Fatalf("quote_approval wrong: %#v", st["quote_approval"])
	}

	acct, ok := st["account"].(map[string]any)
	if !ok {
		t.Fatal("account object missing")
	}
	if acct["display_name"] != "user" {
		t.Errorf("display_name should fall back to username, got %#v", acct["display_name"])
	}
	if acct["acct"] != "user" {
		t.Errorf("acct must remain local username without domain, got %#v", acct["acct"])
	}
	if acct["url"] != "https://www.instagram.com/user/" || acct["uri"] != "https://www.instagram.com/user/" {
		t.Errorf("account URLs wrong: url=%#v uri=%#v", acct["url"], acct["uri"])
	}
	for _, k := range []string{"note", "avatar_description", "header", "header_static", "header_description", "discoverable", "created_at", "last_status_at", "hide_collections"} {
		if acct[k] != nil {
			t.Errorf("account %s must be JSON null when unavailable, got %#v", k, acct[k])
		}
	}
	for _, k := range []string{"avatar", "avatar_static"} {
		if acct[k] != "https://oginstagram.com"+defaultAvatarPath {
			t.Errorf("account %s must fall back to the default avatar, got %#v", k, acct[k])
		}
	}
	if acct["indexable"] != false || acct["show_media"] != false || acct["show_media_replies"] != false || acct["show_featured"] != false {
		t.Errorf("account boolean defaults wrong: %#v", acct)
	}
	fa, ok := acct["feature_approval"].(map[string]any)
	if !ok || fa["current_user"] != "missing" {
		t.Fatalf("feature_approval wrong: %#v", acct["feature_approval"])
	}
}

func TestBuildMastodonProfileStatusUsesKnownProfileFields(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram"}}
	taken := time.Date(2026, 4, 23, 10, 1, 13, 0, time.UTC)
	p := Profile{
		Username:       "user",
		UserID:         "123",
		FullName:       "User Name",
		Biography:      "hello #tag",
		ProfilePic:     "https://cdn/avatar.jpg",
		FollowerCount:  7,
		FollowingCount: 3,
		MediaCount:     11,
		RecentMedia:    []ProfileMedia{{ID: "m1", Thumbnail: "https://cdn/thumb.jpg", Width: 640, Height: 480, TakenAt: taken}},
	}

	var st map[string]any
	if err := json.Unmarshal(a.buildMastodonProfileStatus("https://oginstagram.com", p), &st); err != nil {
		t.Fatal(err)
	}
	if st["created_at"] != "2026-04-23T10:01:13.000Z" {
		t.Fatalf("status created_at = %#v", st["created_at"])
	}
	acct := st["account"].(map[string]any)
	if acct["id"] != "123" || acct["display_name"] != "User Name" {
		t.Errorf("account identity wrong: %#v", acct)
	}
	if acct["created_at"] != nil {
		t.Errorf("account creation date is unknown and must be null, got %#v", acct["created_at"])
	}
	if acct["last_status_at"] != "2026-04-23" {
		t.Errorf("last_status_at = %#v", acct["last_status_at"])
	}
	if acct["followers_count"] != float64(7) || acct["following_count"] != float64(3) || acct["statuses_count"] != float64(11) {
		t.Errorf("account counts wrong: %#v", acct)
	}
	if acct["note"] == nil || acct["avatar"] != "https://cdn/avatar.jpg" {
		t.Errorf("known profile fields missing: %#v", acct)
	}
}

func TestVideoDisplaySize(t *testing.T) {
	cases := []struct {
		name  string
		att   Attachment
		wantW int
		wantH int
	}{
		{"oversized halved", Attachment{Kind: "video", Width: 2160, Height: 3840}, 1080, 1920},
		{"tiny doubled", Attachment{Kind: "video", Width: 320, Height: 320}, 640, 640},
		{"normal unchanged", Attachment{Kind: "video", Width: 1080, Height: 1080}, 1080, 1080},
		{"tall not doubled when one axis large", Attachment{Kind: "video", Width: 320, Height: 640}, 320, 640},
		{"image never scaled", Attachment{Kind: "image", Width: 4000, Height: 4000}, 4000, 4000},
	}
	for _, c := range cases {
		if w, h := videoDisplaySize(c.att); w != c.wantW || h != c.wantH {
			t.Errorf("%s: videoDisplaySize = %dx%d, want %dx%d", c.name, w, h, c.wantW, c.wantH)
		}
	}
}

func TestCaptionHTMLLinkifiesEntities(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"mention", "hi @bob.smith", `hi <a href="https://www.instagram.com/bob.smith">@bob.smith</a>`},
		{"hashtag", "love #food", `love <a href="https://www.instagram.com/explore/search/keyword/?q=%23food">#food</a>`},
		{"hashtag unicode", "오늘 #일상", `오늘 <a href="https://www.instagram.com/explore/search/keyword/?q=%23%EC%9D%BC%EC%83%81">#일상</a>`},
		{"hashtag numeric start", "#100days", `<a href="https://www.instagram.com/explore/search/keyword/?q=%23100days">#100days</a>`},
		{"email linked", "mail me at foo@bar.com", `mail me at <a href="mailto:foo@bar.com">foo@bar.com</a>`},
		{"escaped quote not a hashtag", "it's #real", `it&#39;s <a href="https://www.instagram.com/explore/search/keyword/?q=%23real">#real</a>`},
		{"pure number is a hashtag on Instagram", "win #100", `win <a href="https://www.instagram.com/explore/search/keyword/?q=%23100">#100</a>`},
		{"underscore-only not a hashtag", "a #___ b", "a #___ b"},
		{"adjacent hashtags both link", "#a#b", `<a href="https://www.instagram.com/explore/search/keyword/?q=%23a">#a</a><a href="https://www.instagram.com/explore/search/keyword/?q=%23b">#b</a>`},
		{"hashtag attached to word not linked", "abc#def", "abc#def"},
		{"angle brackets escaped", "a <b> #x", `a &lt;b&gt; <a href="https://www.instagram.com/explore/search/keyword/?q=%23x">#x</a>`},
		{"scheme url", "see https://example.com/x", `see <a href="https://example.com/x">https://example.com/x</a>`},
		{"url trailing dot trimmed", "go https://example.com.", `go <a href="https://example.com">https://example.com</a>.`},
		{"www prepended scheme", "at www.example.com now", `at <a href="https://www.example.com">www.example.com</a> now`},
		{"fuzzy domain with path", "watch youtube.com/abc here", `watch <a href="https://youtube.com/abc">youtube.com/abc</a> here`},
		{"non-tld not linked", "open report.pdf please", "open report.pdf please"},
	}
	for _, c := range cases {
		if got := captionHTML(c.in); got != c.want {
			t.Errorf("%s: captionHTML(%q) =\n  %q\nwant\n  %q", c.name, c.in, got, c.want)
		}
	}
}

func TestBuildEmbedHTMLScalesVideoDimensions(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram", BrandColor: "#ff0069"}}
	post := Post{
		Shortcode: "CODE", Username: "user", FullName: "User",
		Attachments: []Attachment{{Kind: "video", Width: 2160, Height: 3840}},
	}
	html := a.buildEmbedHTML("https://oginstagram.com", "Discordbot", post, "p", 0, false, false)
	for _, tag := range []string{
		`property="og:video:width" content="1080"`,
		`property="og:video:height" content="1920"`,
	} {
		if !strings.Contains(html, tag) {
			t.Errorf("video embed missing scaled tag %s\n%s", tag, html)
		}
	}

	if strings.Contains(html, "twitter:player") || strings.Contains(html, `twitter:card" content="player"`) {
		t.Errorf("video embed must not use a Twitter player card:\n%s", html)
	}
}
