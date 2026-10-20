package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var componentScriptRE = regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script\s*>`)

func componentPayload(t *testing.T, document string) map[string]any {
	t.Helper()
	var payload map[string]any
	for _, script := range componentScriptRE.FindAllStringSubmatch(document, -1) {
		if !strings.Contains(script[1], `id="discord:component-embed"`) {
			continue
		}
		if payload != nil {
			t.Fatal("more than one Component Embed script")
		}
		if !strings.Contains(script[1], `type="application/vnd.discord.component-embed+json"`) {
			t.Fatalf("incorrect Component Embed script MIME: %s", script[1])
		}
		if len(script[2]) > discordComponentMaxBytes {
			t.Fatalf("Component Embed exceeds the %d-byte wire limit: %d bytes", discordComponentMaxBytes, len(script[2]))
		}
		if !utf8.ValidString(script[2]) {
			t.Fatal("Component Embed is not valid UTF-8")
		}
		if err := json.Unmarshal([]byte(script[2]), &payload); err != nil {
			t.Fatalf("invalid Component Embed JSON: %v", err)
		}
	}
	if payload == nil {
		t.Fatal("Component Embed script missing")
	}
	if len(payload) != 1 {
		t.Fatalf("unexpected Component Embed envelope fields: %v", payload)
	}
	container, ok := payload["component"].(map[string]any)
	if !ok || container["type"] != float64(17) || container["accent_color"] != float64(0xff0069) {
		t.Fatalf("invalid Component Embed root container: %v", payload)
	}
	components, ok := container["components"].([]any)
	if !ok || len(components) == 0 {
		t.Fatal("Component Embed has no content")
	}
	count, containers, galleryItems := 0, 0, 0
	walkComponentJSON(payload, func(object map[string]any) {
		if _, found := object["id"]; found {
			t.Error("Component Embed must omit component ids")
		}
		if _, found := object["custom_id"]; found {
			t.Error("Component Embed must not contain interactive custom ids")
		}
		kind, found := object["type"]
		if !found {
			return
		}
		count++
		switch kind {
		case float64(1), float64(9), float64(10), float64(11), float64(14):
		case float64(2):
			// Any other key invalidates the whole payload.
			for key := range object {
				if !slices.Contains([]string{"id", "type", "url", "style", "label", "emoji", "disabled"}, key) {
					t.Errorf("button has a key Discord rejects: %q in %v", key, object)
				}
			}
			if object["style"] != float64(5) || object["label"] == nil && object["emoji"] == nil {
				t.Errorf("button must be a link button with a label or emoji: %v", object)
			}
		case float64(12):
			items, _ := object["items"].([]any)
			galleryItems += len(items)
		case float64(17):
			containers++
		default:
			t.Errorf("unsupported component type: %v", kind)
		}
	})
	if count > 40 {
		t.Errorf("Component Embed exceeds 40 components: %d", count)
	}
	if containers != 1 {
		t.Errorf("Component Embed must not nest a Container: %d containers", containers)
	}
	if galleryItems > 10 {
		t.Errorf("Component Embed exceeds 10 media gallery items across the embed: %d", galleryItems)
	}
	return payload
}

func walkComponentJSON(value any, visit func(map[string]any)) {
	switch value := value.(type) {
	case map[string]any:
		visit(value)
		for _, child := range value {
			walkComponentJSON(child, visit)
		}
	case []any:
		for _, child := range value {
			walkComponentJSON(child, visit)
		}
	}
}

func componentsOfType(payload map[string]any, kind int) []map[string]any {
	var result []map[string]any
	walkComponentJSON(payload, func(object map[string]any) {
		if object["type"] == float64(kind) {
			result = append(result, object)
		}
	})
	return result
}

func componentMediaPaths(t *testing.T, payload map[string]any, signer offloadSigner) []string {
	t.Helper()
	var paths []string
	for _, gallery := range componentsOfType(payload, 12) {
		items, ok := gallery["items"].([]any)
		if !ok || len(items) == 0 || len(items) > 10 {
			t.Fatalf("invalid media gallery size: %v", gallery)
		}
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("invalid media gallery item: %v", item)
			}
			media, ok := object["media"].(map[string]any)
			if !ok {
				t.Fatalf("gallery item has no media: %v", item)
			}
			raw, _ := media["url"].(string)
			parsed, err := url.Parse(raw)
			if err != nil || !strings.HasPrefix(parsed.Path, "/offload/") || !signer.authorize(parsed, time.Now()) {
				t.Fatalf("gallery URL is not an authorized offload capability: %q", raw)
			}
			paths = append(paths, parsed.Path)
		}
	}
	return paths
}

func componentTestPost() Post {
	return Post{
		Shortcode: "ABC123", Username: "alice", FullName: "Alice", ProfilePic: "https://cdn.example/avatar.jpg",
		Caption: "A caption", StatsLine: "❤️ 42  💬 7", CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
		Attachments: []Attachment{
			{Kind: "image", URL: "https://cdn.example/1.jpg", Width: 1080, Height: 1350},
			{Kind: "video", URL: "https://cdn.example/2.mp4", Thumbnail: "https://cdn.example/2.jpg", Width: 1080, Height: 1920},
			{Kind: "image", URL: "https://cdn.example/3.jpg", Width: 1080, Height: 1080},
			{Kind: "video", URL: "https://cdn.example/4.mp4", Thumbnail: "https://cdn.example/4.jpg", Width: 1080, Height: 1920},
		},
	}
}

func TestComponentEmbedAuthorMarkdownLabels(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	for _, tc := range []struct {
		name, plain string
	}{
		{"Chelsea Yamase🌊✨🤸🏽 Nature travel + mindset", "Chelsea Yamase🌊✨🤸🏽 Nature travel + mindset"},
		{"Alice (사진)", "Alice (사진)"},
		{"Alice | photos", `Alice \| photos`},
		{"Alice #-+! photos", "Alice #-+! photos"},
		{`Alice\Photos`, `Alice\\Photos`},
		{"Alice_Photos", `Alice\_Photos`},
		{"Alice ||spoiler||", `Alice \|\|spoiler\|\|`},
		{"Alice [click](https://evil.test)", `Alice \[click\](https\://evil.test)`},
		{"Alice `code`", "Alice \\`code\\`"},
		{"Alice **bold**", `Alice \*\*bold\*\*`},
		{"Alice ~strike~", `Alice \~strike\~`},
		{"Alice <:verified:123456789012345678>", `Alice \<:verified:123456789012345678>`},
		{"Alice\u200bhidden", "Alice\u200bhidden"},
		{"Alice\u034fhidden", "Alice\u034fhidden"},
		{"\u3000\u00a0 ", "alice"},
	} {
		post.FullName = tc.name
		for _, document := range []string{
			a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false),
			a.buildProfileEmbedHTML("https://oginstagram.com", Profile{Username: post.Username, FullName: post.FullName}, false, false),
			a.storyComponentEmbed("https://oginstagram.com", Story{Username: post.Username, FullName: post.FullName, ID: "123", Media: post.Attachments[0]}, false, false),
		} {
			payload := componentPayload(t, document)
			// The name is never a masked link; the handle line carries the profile link.
			want := "**" + tc.plain + "**\n[@alice](https://www.instagram.com/alice/)"
			found := false
			for _, display := range componentsOfType(payload, 10) {
				if content := display["content"].(string); content == want || strings.HasPrefix(content, want+"\n\n") {
					found = true
				}
			}
			if !found {
				t.Fatalf("author %q must be escaped plain text above a linked handle; want %q in %v", tc.name, want, payload)
			}
		}
	}
}

func TestComponentEmbedAuthorUsernameAndVerifiedBadge(t *testing.T) {
	const verifiedID = "123456789012345678"
	for _, tc := range []struct {
		name, modelJSON, emojiID string
		wantBadge                bool
	}{
		{"verified", `{"IsVerified":true}`, verifiedID, true},
		{"unverified", `{"IsVerified":false}`, verifiedID, false},
		{"missing upstream field or old cache", `{}`, verifiedID, false},
		{"no configured icon", `{"IsVerified":true}`, "", false},
		{"invalid configured icon", `{"IsVerified":true}`, "bad:id", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{cfg: Config{DiscordVerifiedEmojiID: tc.emojiID}, offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
			post := componentTestPost()
			post.Username, post.FullName = "alice_photos", "Alice"
			profile := Profile{Username: post.Username, FullName: post.FullName}
			story := Story{ID: "123", Username: post.Username, FullName: post.FullName, Media: post.Attachments[0]}
			for _, model := range []any{&post, &profile, &story} {
				if err := json.Unmarshal([]byte(tc.modelJSON), model); err != nil {
					t.Fatal(err)
				}
			}
			for _, gallery := range []bool{false, true} {
				for _, document := range []string{
					a.postComponentEmbed("https://oginstagram.com", post, "p", 0, false, gallery, false),
					a.profileComponentEmbed("https://oginstagram.com", profile, gallery, false),
					a.storyComponentEmbed("https://oginstagram.com", story, gallery, false),
				} {
					payload := componentPayload(t, document)
					want := "**Alice**"
					if tc.wantBadge {
						want += " <:verified:" + verifiedID + ">"
					}
					want += "\n[@alice_photos](https://www.instagram.com/alice_photos/)"
					found := false
					for _, display := range componentsOfType(payload, 10) {
						content := display["content"].(string)
						if strings.HasPrefix(content, "**") {
							found = true
							if content != want && !strings.HasPrefix(content, want+"\n\n") {
								t.Errorf("author must put escaped handle and genuine badge outside the name link: got %q, want %q", content, want)
							}
						}
						if !tc.wantBadge && (strings.Contains(content, "<:verified:") || strings.ContainsAny(content, "✓✔☑✅")) {
							t.Errorf("missing verified icon must not gain a substitute badge: %q", content)
						}
					}
					if !found {
						t.Fatal("multiline author heading missing")
					}
				}
			}
		})
	}
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	post.Username, post.FullName = "alice_photos", ""
	payload := componentPayload(t, a.postComponentEmbed("https://oginstagram.com", post, "p", 0, false, false, false))
	want := "**alice\\_photos**\n[@alice_photos](https://www.instagram.com/alice_photos/)"
	for _, display := range componentsOfType(payload, 10) {
		if strings.HasPrefix(display["content"].(string), want+"\n\n") {
			return
		}
	}
	t.Errorf("missing full name must fall back to an escaped username outside the link: want %q", want)
}

func TestComponentEmbedMixedMediaAndLegacyMetadata(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false)
	payload := componentPayload(t, document)
	if paths := componentMediaPaths(t, payload, a.offloadSigner); strings.Join(paths, ",") != "/offload/ABC123/1,/offload/ABC123/2,/offload/ABC123/3,/offload/ABC123/4" {
		t.Fatalf("mixed media order or video was lost: %v", paths)
	}
	if len(componentsOfType(payload, 9)) != 2 || len(componentsOfType(payload, 11)) != 1 {
		t.Error("Component Embed is missing the author section, avatar, or footer section")
	}
	for _, tag := range []string{`property="og:title"`, `property="og:image"`, `name="twitter:card"`, `rel="canonical"`} {
		if !strings.Contains(document, tag) {
			t.Errorf("legacy metadata missing: %s", tag)
		}
	}
	if want := strings.Join(apLinkTags(postRefs("https://oginstagram.com", post, "p", 0, false)), ""); !strings.Contains(document, want) || strings.Count(document, "activity+json") != 2 {
		t.Errorf("want one alternate and one author link, %s", want)
	}
	buttons := componentsOfType(payload, 2)
	if len(buttons) != 1 || buttons[0]["style"] != float64(5) || buttons[0]["url"] != instagramPostURL("p", post.Shortcode, 0, false) {
		t.Fatalf("original post link is not a link-only button: %v", buttons)
	}
	walkComponentJSON(payload, func(object map[string]any) {
		raw, ok := object["url"].(string)
		if !ok || strings.HasPrefix(raw, instagramOrigin) {
			return
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host != "oginstagram.com" || !a.offloadSigner.authorize(parsed, time.Now()) {
			t.Errorf("media or avatar escaped signed local delivery: %q", raw)
		}
	})
}

func TestComponentEmbedButtonsAndFooter(t *testing.T) {
	const baseURL, brandID = "https://oginstagram.com", "234567890123456789"
	post := componentTestPost()
	post.Caption = strings.Repeat("🌸긴 캡션<&> ", 1500)
	profile := Profile{Username: "alice", FullName: "Alice", Biography: post.Caption, ProfilePic: post.ProfilePic, MediaCount: 12, FollowerCount: 34}
	story := Story{ID: "123", Username: "alice", FullName: "Alice", Caption: post.Caption, ProfilePic: post.ProfilePic, CreatedAt: post.CreatedAt, Media: post.Attachments[1]}
	storyOrigin := storyOriginURL(story.Username, story.ID)
	for _, icon := range []struct{ name, id, want string }{
		{"configured", brandID, "<:OGInstagram:" + brandID + "> "},
		{"missing", "", ""},
		{"invalid", "not-an-emoji-id", ""},
	} {
		a := &App{cfg: Config{DiscordBrandEmojiID: icon.id}, offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
		for _, tc := range []struct {
			name, origin, stats string
			created             time.Time
			render              func(gallery, support bool) string
		}{
			{"post", instagramPostURL("reel", post.Shortcode, 1, true), "❤️ **42**  💬 **7**", post.CreatedAt, func(gallery, support bool) string {
				return a.buildEmbedHTML(baseURL, post, "reel", 1, true, gallery, support)
			}},
			{"profile", profileURL(profile.Username), "📝 **12** 👤 **34**", time.Time{}, func(gallery, support bool) string {
				return a.buildProfileEmbedHTML(baseURL, profile, gallery, support)
			}},
			{"story", storyOrigin, "", story.CreatedAt, func(gallery, support bool) string {
				return a.buildStoryEmbedHTML(baseURL, storyOrigin, story,
					a.storyOffloadURL(baseURL, story.Username, story.ID, false), a.storyOffloadURL(baseURL, story.Username, story.ID, true), gallery, support)
			}},
		} {
			for _, support := range []bool{false, true} {
				for _, gallery := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s support=%v gallery=%v", icon.name, tc.name, support, gallery)
					payload := componentPayload(t, tc.render(gallery, support)) // Includes the final 3,000-byte wire check.
					buttons, displays := componentsOfType(payload, 2), componentsOfType(payload, 10)
					if gallery {
						if len(buttons) != 0 || len(displays) != 1 || !strings.HasPrefix(displays[0]["content"].(string), "**") || strings.Contains(displays[0]["content"].(string), "<t:") {
							t.Errorf("%s: gallery must keep only author text and media, with no footer or buttons: %v", name, payload)
						}
						continue
					}
					wantBrand := "-# " + icon.want + "OGInstagram"
					if !tc.created.IsZero() {
						wantBrand += fmt.Sprintf(" · <t:%d:s>", tc.created.Unix())
					}
					header, brands := "", 0
					for _, display := range displays {
						switch content := display["content"].(string); {
						case strings.HasPrefix(content, "**"):
							header = content
						case content == wantBrand:
							brands++
						case strings.Contains(content, "<t:") || strings.Contains(content, "OGInstagram"):
							t.Errorf("%s: timestamp and branding must appear only in the footer: %q", name, content)
						}
					}
					if brands != 1 || tc.stats != "" && !strings.HasSuffix(header, "\n\n"+tc.stats) {
						t.Errorf("%s: want one footer %q and stats %q ending the heading %q", name, wantBrand, tc.stats, header)
					}
					wantButtons := 1
					if support {
						wantButtons = 2
					}
					// Support: brand line, divider, then both buttons in a row; else the lone button beside the brand.
					children := payload["component"].(map[string]any)["components"].([]any)
					last := children[len(children)-1].(map[string]any)
					if len(buttons) != wantButtons || buttons[0]["url"] != tc.origin ||
						support && (children[len(children)-2].(map[string]any)["type"] != float64(14) || last["type"] != float64(1)) ||
						!support && (last["type"] != float64(9) || last["accessory"] == nil) {
						t.Errorf("%s: buttons or footer layout: %v", name, children)
					}
				}
			}
		}
	}
}

func TestComponentEmbedSelectionAndGalleryLimit(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	for _, index := range []int{1, 3, 999} {
		document := a.buildEmbedHTML("https://oginstagram.com", post, "reel", index, true, false, false)
		paths := componentMediaPaths(t, componentPayload(t, document), a.offloadSigner)
		want := fmt.Sprintf("/offload/ABC123/%d", min(index, 3)+1)
		if len(paths) != 1 || paths[0] != want {
			t.Errorf("selected index %d = %v, want %s", index, paths, want)
		}
		if !strings.Contains(document, `property="og:video"`) {
			t.Error("selected video lost legacy video metadata")
		}
	}
	for len(post.Attachments) < 15 {
		post.Attachments = append(post.Attachments, post.Attachments[0])
	}
	document := a.buildEmbedHTML("https://g.oginstagram.com", post, "p", 0, false, true, false)
	payload := componentPayload(t, document)
	paths := componentMediaPaths(t, payload, a.offloadSigner)
	if len(paths) != 10 {
		t.Fatalf("gallery should retain the first ten media within the byte limit: %v", paths)
	}
	for i, path := range paths {
		if want := fmt.Sprintf("/offload/ABC123/%d", i+1); path != want {
			t.Errorf("gallery item %d = %q, want %q", i, path, want)
		}
	}
	if len(componentsOfType(payload, 9)) != 1 || len(componentsOfType(payload, 2)) != 0 || strings.Contains(document, post.Caption) || strings.Contains(document, post.StatsLine) {
		t.Error("gallery-only Component Embed leaked caption, stats, or controls")
	}
}

func TestComponentEmbedSafeJSONAndByteBudget(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	post.Attachments = post.Attachments[:1]
	post.Caption = `</script><img src=x onerror=alert(1)> **caption** [trick](javascript:alert(1))`
	document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false)
	payload := componentPayload(t, document)
	if strings.Count(document, "</script>") != 1 || strings.Contains(document, "<img src=x") {
		t.Error("untrusted caption escaped the JSON script")
	}
	var text strings.Builder
	for _, display := range componentsOfType(payload, 10) {
		text.WriteString(display["content"].(string))
	}
	if strings.Contains(text.String(), "**caption**") || strings.Contains(text.String(), "[trick](javascript:") {
		t.Error("caption Markdown was interpreted as author-supplied formatting or a masked link")
	}
	post.Caption = strings.Repeat("🌸한글<&>😀 ", 2000)
	document = a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false)
	payload = componentPayload(t, document)
	if len(componentMediaPaths(t, payload, a.offloadSigner)) != 1 {
		t.Error("large caption removed the only media item")
	}
	for _, display := range componentsOfType(payload, 10) {
		content, _ := display["content"].(string)
		if !utf8.ValidString(content) || strings.ContainsRune(content, utf8.RuneError) {
			t.Errorf("caption truncation damaged Unicode: %q", content)
		}
	}
	post = componentTestPost()
	document = a.buildEmbedHTML("https://oginstagram.com/"+strings.Repeat("x", 600), post, "p", 0, false, false, false)
	payload = componentPayload(t, document)
	galleries := componentsOfType(payload, 12)
	if len(galleries) != 1 {
		t.Fatalf("long signed URLs must keep one media gallery: %v", payload)
	}
	if items := galleries[0]["items"].([]any); len(items) == 0 || len(items) >= len(post.Attachments) {
		t.Errorf("long signed URLs should reduce media count to fit the wire limit: %d", len(items))
	}
}

func TestComponentEmbedCaptionLinks(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	post.Attachments = post.Attachments[:1]
	post.Caption = "Links: @alice #food https://my-site.test/a_b?q=x&z=1 https://example.com/path_(part) javascript:alert(1)"
	payload := componentPayload(t, a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false))
	var caption string
	for _, display := range componentsOfType(payload, 10) {
		if content, _ := display["content"].(string); strings.HasPrefix(content, "Links:") {
			caption = content
		}
	}
	for _, target := range []string{
		"https://www.instagram.com/alice",
		"https://www.instagram.com/explore/search/keyword/?q=%23food",
		"https://my-site.test/a_b?q=x&z=1",
		"https://example.com/path_%28part%29",
	} {
		if !strings.Contains(caption, "]("+target+")") && !strings.Contains(caption, "<"+target+">") {
			t.Errorf("caption did not preserve the complete link destination %q: %s", target, caption)
		}
	}
	if strings.Contains(caption, "](javascript:") || !strings.Contains(caption, "javascript:alert(1)") {
		t.Errorf("unsafe scheme text must remain escaped literal text: %s", caption)
	}
	post.Caption = "Partial link: https://example.com/" + strings.Repeat("a", 6000)
	payload = componentPayload(t, a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false))
	partialFound := false
	for _, display := range componentsOfType(payload, 10) {
		content, _ := display["content"].(string)
		if strings.HasPrefix(content, "Partial link:") {
			partialFound = true
			if strings.Contains(content, "https://example.com/") || !strings.HasSuffix(content, "…") {
				t.Errorf("truncation must omit an incomplete URL before Discord can auto-link it: %s", content)
			}
		}
	}
	if !partialFound {
		t.Error("truncating an oversized link discarded the preceding caption")
	}
}

func TestComponentEmbedCaptionMarkdownLabels(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	post.Attachments = post.Attachments[:1]
	for _, tc := range []struct{ input, want string }{
		{"@alice #food", "[@alice](https://www.instagram.com/alice) [#food](https://www.instagram.com/explore/search/keyword/?q=%23food)"},
		{"https://my-site.test/path(part)", `https\://my-site.test/path(part) [↗](https://my-site.test/path%28part%29)`},
		{"https://my-site.test/a?q=x&z=1", "<https://my-site.test/a?q=x&z=1>"},
		{"https://my-site.test/a_b?q=x&z=1", "<https://my-site.test/a_b?q=x&z=1>"},
		{"@alice_photos #food_tag", "[@alice_photos](https://www.instagram.com/alice_photos) [#food_tag](https://www.instagram.com/explore/search/keyword/?q=%23food_tag)"},
		{"@favrit1._j.nyang2 @x_y_z", "[@favrit1._j.nyang2](https://www.instagram.com/favrit1._j.nyang2) [@x_y_z](https://www.instagram.com/x_y_z)"},
		{"@_alice_ @a__b", "[@_alice_](https://www.instagram.com/_alice_) [@a__b](https://www.instagram.com/a__b)"},
		{"https://example.com/||secret||", "<https://example.com/||secret||>"},
		{`https://example.com/a\b`, `https\://example.com/a\\b [↗](https://example.com/a%5Cb)`},
		{"[click](https://example.com/x)", `\[click\](<https://example.com/x>)`},
		{"Use `code` and @alice", "Use \\`code\\` and [@alice](https://www.instagram.com/alice)"},
		{"# Big\n-# small\n- item\n+ plus\n1. first\n> quote\nmid - # + > 1. stays", "\\# Big\n\\-# small\n\\- item\n\\+ plus\n1\\. first\n\\> quote\nmid - # + > 1. stays"},
		{"No badge <:verified:123456789012345678>", `No badge \<:verified:123456789012345678>`},
	} {
		post.Caption = tc.input
		payload := componentPayload(t, a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false))
		found := false
		for _, display := range componentsOfType(payload, 10) {
			if display["content"] == tc.want {
				found = true
			}
		}
		if !found {
			t.Errorf("caption %q must link labels unless the label would break the link syntax; want %q in %v", tc.input, tc.want, payload)
		}
	}
}

func TestComponentEmbedKeepsCaptionThatExactlyFits(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	post.Attachments = post.Attachments[:1]
	wire := func(caption string) (string, map[string]any) {
		post.Caption = caption
		document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, false)
		return componentScriptRE.FindStringSubmatch(document)[2], componentPayload(t, document)
	}
	baseline, _ := wire("x")
	caption := strings.Repeat("x", 3000-len(baseline)+1)
	body, payload := wire(caption)
	if len(body) != 3000 {
		t.Errorf("full caption should exactly fill the wire budget, got %d bytes", len(body))
	}
	if long, _ := wire(strings.Repeat("x", 6000)); len(long) <= 3000-16 {
		t.Errorf("a cut caption should nearly fill the wire budget, got %d bytes", len(long))
	}
	for _, display := range componentsOfType(payload, 10) {
		if display["content"] == caption {
			return
		}
	}
	t.Error("a complete caption that fits exactly was unnecessarily truncated")
}

func TestComponentEmbedFallsBackToOpenGraph(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	for _, document := range []string{
		a.buildEmbedHTML("javascript:alert(1)", post, "p", 0, false, false, false),
		a.buildEmbedHTML("https://oginstagram.com/"+strings.Repeat("x", 1500), post, "p", 0, false, false, true),
		a.buildEmbedHTML("https://oginstagram.com/"+strings.Repeat("x", 4000), post, "p", 0, false, false, false),
		a.buildStatusEmbedHTML("https://oginstagram.com", "https://www.instagram.com/p/ABC123/", "Unavailable", "Try again later"),
	} {
		r := cardResp(document, "", "")
		if strings.Contains(document, `id="discord:component-embed"`) ||
			!strings.Contains(document, `property="og:title"`) || r.status != http.StatusOK || r.headers["X-OGInstagram-Embed"] != "opengraph" {
			t.Errorf("an unusable Component Embed must leave a plain Open Graph card: %v %.300s", r.headers, document)
		}
	}
}

func TestComponentEmbedProfileAndStory(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	profile := Profile{Username: "alice", FullName: "Alice", Biography: "private biography", ProfilePic: "https://cdn.example/avatar.jpg", IsPrivate: true}
	document := a.buildProfileEmbedHTML("https://oginstagram.com", profile, false, false)
	payload := componentPayload(t, document)
	biographyFound, privacyFound := false, false
	for _, display := range componentsOfType(payload, 10) {
		content, _ := display["content"].(string)
		biographyFound = biographyFound || strings.Contains(content, profile.Biography)
		privacyFound = privacyFound || strings.Contains(content, "🔒 This profile is private.")
	}
	if !biographyFound || !privacyFound {
		t.Error("private profile Component Embed lost its biography or privacy notice")
	}
	profile.RecentMedia = []ProfileMedia{{Thumbnail: "https://cdn.example/1.jpg"}, {Thumbnail: "https://cdn.example/2.jpg"}}
	document = a.buildProfileEmbedHTML("https://g.oginstagram.com", profile, true, false)
	payload = componentPayload(t, document)
	if len(componentMediaPaths(t, payload, a.offloadSigner)) == 0 || len(componentsOfType(payload, 9)) != 1 || strings.Contains(document, profile.Biography) || strings.Contains(document, profilePrivateNotice) {
		t.Error("gallery profile must expose media without biography or text")
	}
	story := Story{ID: "123", Username: "alice", FullName: "Alice", ProfilePic: "https://cdn.example/avatar.jpg", Caption: "story caption", Media: componentTestPost().Attachments[1]}
	for _, gallery := range []bool{false, true} {
		baseURL := "https://oginstagram.com"
		document = a.buildStoryEmbedHTML(baseURL, storyOriginURL(story.Username, story.ID), story,
			a.storyOffloadURL(baseURL, story.Username, story.ID, false), a.storyOffloadURL(baseURL, story.Username, story.ID, true), gallery, false)
		payload = componentPayload(t, document)
		paths := componentMediaPaths(t, payload, a.offloadSigner)
		if len(paths) != 1 || paths[0] != "/offload/story/alice/123" || !strings.Contains(document, `property="og:video"`) {
			t.Errorf("story video did not retain media or legacy video metadata: %v", paths)
		}
		if gallery && (len(componentsOfType(payload, 9)) != 1 || strings.Contains(document, story.Caption)) {
			t.Error("gallery story leaked caption or text components")
		}
	}
}

func TestGatewayCardIgnoresUserAgent(t *testing.T) {
	g := testGateway(t)
	post := componentTestPost()
	g.app.posts.storeLocal(post.Shortcode, &cacheEntry[Post]{value: post, expiresAt: time.Now().Add(time.Hour)})
	// The edge caches one copy per URL for every bot, so no User-Agent may change the card.
	for _, host := range []string{"oginstagram.com", "g.oginstagram.com"} {
		first := ""
		for _, ua := range []string{"Discordbot/2.0", "TelegramBot (like TwitterBot)"} {
			r := publicRequest("GET", "/p/ABC123?e=s")
			r.Host = host
			r.Header.Set("User-Agent", ua)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			body := w.Body.String()
			componentPayload(t, body)
			// Gallery cards carry no buttons at all.
			if w.Code != http.StatusOK || w.Header().Get("X-OGInstagram-Embed") != "component" || strings.Contains(body, "https://ko-fi.com/seirenkr") != (host == "oginstagram.com") {
				t.Errorf("%s for %q = %d %v", host, ua, w.Code, w.Header())
			}
			if first != "" && body != first {
				t.Errorf("%s differs for %q", host, ua)
			}
			first = body
		}
	}
	for _, path := range []string{"/p/ABC123/2", "/p/ABC123?img_index=2"} {
		result := serveApp(t, g.app, publicRequest("GET", path))
		paths := componentMediaPaths(t, componentPayload(t, string(result.body)), g.app.offloadSigner)
		if len(paths) != 1 || paths[0] != "/offload/ABC123/2" {
			t.Errorf("component URL selection for %s: %v", path, paths)
		}
	}
	r := publicRequest("POST", "/api/embed")
	r.Body = io.NopCloser(strings.NewReader(`{"path":"/p/ABC123"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://oginstagram.com")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Cookie", "cf_clearance=component-test")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || strings.Contains(w.Body.String(), `id="og-status"`) {
		t.Errorf("frontend preview must return the bot card itself: %d %s", w.Code, w.Body.String())
	}
	componentPayload(t, w.Body.String())
}
