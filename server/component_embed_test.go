package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
		if len(script[2]) > 3000 {
			t.Fatalf("Component Embed exceeds the 3000-byte wire limit: %d bytes", len(script[2]))
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
	count := 0
	walkComponentJSON(payload, func(object map[string]any) {
		if _, found := object["id"]; found {
			t.Error("Component Embed must omit component ids")
		}
		if _, found := object["custom_id"]; found {
			t.Error("Component Embed must not contain interactive custom ids")
		}
		if kind, found := object["type"]; found {
			count++
			switch kind {
			case float64(1), float64(2), float64(9), float64(10), float64(11), float64(12), float64(14), float64(17):
			default:
				t.Errorf("unsupported component type: %v", kind)
			}
		}
	})
	if count > 40 {
		t.Errorf("Component Embed exceeds 40 components: %d", count)
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
	} {
		post.FullName = tc.name
		for _, document := range []string{
			a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents),
			a.buildProfileEmbedHTML("https://oginstagram.com", Profile{Username: post.Username, FullName: post.FullName}, false, discordComponents),
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
	document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents)
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
	if strings.Contains(document, `type="application/activity+json"`) {
		t.Error("Component Embed must replace ActivityPub discovery for Discord")
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

func TestComponentEmbedSupportButtonsAndTimestamps(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	const baseURL = "https://oginstagram.com"
	post := componentTestPost()
	post.Caption = strings.Repeat("🌸긴 캡션<&> ", 1500)
	profile := Profile{Username: "alice", FullName: "Alice", Biography: post.Caption, ProfilePic: post.ProfilePic}
	story := Story{ID: "123", Username: "alice", FullName: "Alice", Caption: post.Caption, ProfilePic: post.ProfilePic, CreatedAt: post.CreatedAt, Media: post.Attachments[1]}
	for _, tc := range []struct {
		name, origin string
		created      time.Time
		render       func(bool, discordEmbedVariant) string
	}{
		{"post", instagramPostURL("reel", post.Shortcode, 1, true), post.CreatedAt, func(gallery bool, variant discordEmbedVariant) string {
			return a.buildEmbedHTML(baseURL, post, "reel", 1, true, gallery, variant)
		}},
		{"profile", profileURL(profile.Username), time.Time{}, func(gallery bool, variant discordEmbedVariant) string {
			return a.buildProfileEmbedHTML(baseURL, profile, gallery, variant)
		}},
		{"story", storyOriginURL(story.Username, story.ID), story.CreatedAt, func(gallery bool, variant discordEmbedVariant) string {
			return a.buildStoryEmbedHTML(baseURL, storyOriginURL(story.Username, story.ID), story,
				a.storyOffloadURL(baseURL, story.Username, story.ID, false), a.storyOffloadURL(baseURL, story.Username, story.ID, true),
				storyStatusURL(baseURL, story.Username, story.ID, gallery), gallery, variant)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, variant := range []discordEmbedVariant{discordLegacy, discordComponents, discordComponentsSupport} {
				for _, gallery := range []bool{false, true} {
					document := tc.render(gallery, variant)
					if variant == discordLegacy {
						if strings.Contains(document, `id="discord:component-embed"`) || strings.Contains(document, "https://ko-fi.com/seirenkr") {
							t.Error("legacy embed contains Component Embed support content")
						}
						continue
					}
					payload := componentPayload(t, document) // Includes the final 3,000-byte wire check.
					buttons := componentsOfType(payload, 2)
					if gallery {
						if len(buttons) != 0 || strings.Contains(document, "https://ko-fi.com/seirenkr") {
							t.Error("gallery embed must omit both Instagram and support buttons")
						}
						for _, display := range componentsOfType(payload, 10) {
							if strings.Contains(display["content"].(string), "<t:") {
								t.Error("gallery embed must omit timestamps")
							}
						}
						continue
					}
					wantCount := 1
					if variant == discordComponentsSupport {
						wantCount = 2
					}
					if len(buttons) != wantCount || buttons[0]["label"] != "📷 Instagram" || buttons[0]["url"] != tc.origin {
						t.Fatalf("variant %d buttons or Instagram destination are incorrect: %v", variant, buttons)
					}
					for _, button := range buttons {
						if button["style"] != float64(5) {
							t.Errorf("button must remain a link-only button: %v", button)
						}
					}
					if variant == discordComponentsSupport && (buttons[1]["label"] != "☕ Support me" || buttons[1]["url"] != "https://ko-fi.com/seirenkr") {
						t.Errorf("support button differs from the proposed label and funding destination: %v", buttons[1])
					}
					if !tc.created.IsZero() {
						want := fmt.Sprintf("<t:%d:s>", tc.created.Unix())
						found := false
						for _, display := range componentsOfType(payload, 10) {
							if strings.Contains(display["content"].(string), want) {
								found = true
							}
						}
						if !found {
							t.Errorf("post/story timestamp must use Discord's s style: want %s", want)
						}
					}
				}
			}
		})
	}
}

func TestComponentEmbedStatsHeaderAndBrandFooter(t *testing.T) {
	const brandID = "234567890123456789"
	post := componentTestPost()
	post.Caption = strings.Repeat("🌸긴 캡션<&> ", 1500)
	profile := Profile{Username: "alice", FullName: "Alice", Biography: post.Caption, MediaCount: 12, FollowerCount: 34}
	story := Story{ID: "123", Username: "alice", FullName: "Alice", Caption: post.Caption, CreatedAt: post.CreatedAt, Media: post.Attachments[0]}
	for _, icon := range []struct{ name, id, want string }{
		{"configured", brandID, "<:OGInstagram:" + brandID + "> "},
		{"missing", "", ""},
		{"invalid", "not-an-emoji-id", ""},
	} {
		a := &App{cfg: Config{DiscordBrandEmojiID: icon.id}, offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
		for _, tc := range []struct {
			name, stats string
			created     time.Time
			render      func(bool) string
		}{
			{"post", "❤️ **42**  💬 **7**", post.CreatedAt, func(gallery bool) string {
				return a.postComponentEmbed("https://oginstagram.com", post, "p", 0, false, gallery, true)
			}},
			{"profile without date", "📝 **12** 👤 **34**", time.Time{}, func(gallery bool) string {
				return a.profileComponentEmbed("https://oginstagram.com", profile, gallery, true)
			}},
			{"story", "", story.CreatedAt, func(gallery bool) string {
				return a.storyComponentEmbed("https://oginstagram.com", story, gallery, true)
			}},
		} {
			t.Run(icon.name+"/"+tc.name, func(t *testing.T) {
				for _, gallery := range []bool{false, true} {
					payload := componentPayload(t, tc.render(gallery))
					displays := componentsOfType(payload, 10)
					if gallery {
						if len(displays) != 1 || !strings.HasPrefix(displays[0]["content"].(string), "**") || len(componentsOfType(payload, 2)) != 0 {
							t.Errorf("gallery must keep only author text and media, with no footer or buttons: %v", payload)
						}
						continue
					}
					wantBrand := "-# " + icon.want + "OGInstagram"
					if !tc.created.IsZero() {
						wantBrand += fmt.Sprintf(" · <t:%d:s>", tc.created.Unix())
					}
					header := ""
					for _, display := range displays {
						content := display["content"].(string)
						if strings.HasPrefix(content, "**") {
							header = content
						} else if content != wantBrand && (strings.Contains(content, "<t:") || strings.Contains(content, "OGInstagram")) {
							t.Errorf("timestamp and branding must appear only in the footer: %q", content)
						}
					}
					if tc.stats != "" && !strings.HasSuffix(header, "\n\n"+tc.stats) {
						t.Errorf("stats must be the author heading's last line: %q, want suffix %q", header, tc.stats)
					}
					// These renders use the support variant: brand line, divider, then both buttons.
					children := payload["component"].(map[string]any)["components"].([]any)
					tail := make([]map[string]any, 0, 3)
					for _, child := range children[max(len(children)-3, 0):] {
						tail = append(tail, child.(map[string]any))
					}
					row, _ := tail[2]["components"].([]any)
					if len(tail) != 3 || tail[0]["content"] != wantBrand || tail[1]["type"] != float64(14) || tail[2]["type"] != float64(1) || len(row) != 2 ||
						row[0].(map[string]any)["label"] != "📷 Instagram" || row[1].(map[string]any)["label"] != "☕ Support me" {
						t.Errorf("support footer must be brand %q, a separator, and one row with both buttons: %v", wantBrand, tail)
					}
				}
			})
		}
	}
}

func TestComponentEmbedSelectionAndGalleryLimit(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	for _, index := range []int{1, 3, 999} {
		document := a.buildEmbedHTML("https://oginstagram.com", post, "reel", index, true, false, discordComponents)
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
	document := a.buildEmbedHTML("https://g.oginstagram.com", post, "p", 0, false, true, discordComponents)
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
	document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents)
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
	document = a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents)
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
	document = a.buildEmbedHTML("https://oginstagram.com/"+strings.Repeat("x", 600), post, "p", 0, false, false, discordComponents)
	payload = componentPayload(t, document)
	if items := componentsOfType(payload, 12)[0]["items"].([]any); len(items) == 0 || len(items) >= len(post.Attachments) {
		t.Errorf("long signed URLs should reduce media count to fit the wire limit: %d", len(items))
	}
}

func TestComponentEmbedCaptionLinks(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	post.Attachments = post.Attachments[:1]
	post.Caption = "Links: @alice #food https://my-site.test/a_b?q=x&z=1 https://example.com/path_(part) javascript:alert(1)"
	payload := componentPayload(t, a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents))
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
	payload = componentPayload(t, a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents))
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
		payload := componentPayload(t, a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents))
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
		document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordComponents)
		return componentScriptRE.FindStringSubmatch(document)[2], componentPayload(t, document)
	}
	baseline, _ := wire("x")
	caption := strings.Repeat("x", 3000-len(baseline)+1)
	body, payload := wire(caption)
	if len(body) != 3000 {
		t.Errorf("full caption should exactly fill the wire budget, got %d bytes", len(body))
	}
	for _, display := range componentsOfType(payload, 10) {
		if display["content"] == caption {
			return
		}
	}
	t.Error("a complete caption that fits exactly was unnecessarily truncated")
}

func TestComponentEmbedFallsBackToActivityPub(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	for _, baseURL := range []string{"javascript:alert(1)", "https://oginstagram.com/" + strings.Repeat("x", 1500), "https://oginstagram.com/" + strings.Repeat("x", 4000)} {
		document := a.buildEmbedHTML(baseURL, post, "p", 0, false, false, discordComponents)
		if strings.Contains(document, `id="discord:component-embed"`) || !strings.Contains(document, `type="application/activity+json"`) {
			t.Errorf("unusable component URLs must retain legacy fallback for base URL length %d", len(baseURL))
		}
	}
	document := a.buildEmbedHTML("https://oginstagram.com", post, "p", 0, false, false, discordLegacy)
	if strings.Contains(document, `id="discord:component-embed"`) || !strings.Contains(document, `type="application/activity+json"`) {
		t.Error("legacy renderer must retain ActivityPub discovery")
	}
}

func TestComponentEmbedProfileAndStory(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	profile := Profile{Username: "alice", FullName: "Alice", Biography: "private biography", ProfilePic: "https://cdn.example/avatar.jpg", IsPrivate: true}
	document := a.buildProfileEmbedHTML("https://oginstagram.com", profile, false, discordComponents)
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
	document = a.buildProfileEmbedHTML("https://g.oginstagram.com", profile, true, discordComponents)
	payload = componentPayload(t, document)
	if len(componentMediaPaths(t, payload, a.offloadSigner)) == 0 || len(componentsOfType(payload, 9)) != 1 || strings.Contains(document, profile.Biography) || strings.Contains(document, profilePrivateNotice) {
		t.Error("gallery profile must expose media without biography or text")
	}
	story := Story{ID: "123", Username: "alice", FullName: "Alice", ProfilePic: "https://cdn.example/avatar.jpg", Caption: "story caption", Media: componentTestPost().Attachments[1]}
	for _, gallery := range []bool{false, true} {
		baseURL := "https://oginstagram.com"
		document = a.buildStoryEmbedHTML(baseURL, storyOriginURL(story.Username, story.ID), story,
			a.storyOffloadURL(baseURL, story.Username, story.ID, false), a.storyOffloadURL(baseURL, story.Username, story.ID, true),
			storyStatusURL(baseURL, story.Username, story.ID, gallery), gallery, discordComponents)
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

func TestGatewayComponentEmbedNegotiationAndDirectRoutes(t *testing.T) {
	g := testGateway(t)
	expiry := time.Now().Add(time.Hour)
	post := componentTestPost()
	g.app.posts.storeLocal(post.Shortcode, &cacheEntry[Post]{value: post, expiresAt: expiry})
	g.app.profiles.storeLocal("alice", &cacheEntry[Profile]{value: Profile{Username: "alice", Biography: "bio", ProfilePic: post.ProfilePic}, expiresAt: expiry})
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{value: Story{ID: "123", Username: "alice", Caption: "story", Media: post.Attachments[0]}, expiresAt: expiry})
	g.app.posts.storeLocal("GONE12", &cacheEntry[Post]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: expiry})
	for _, path := range []string{"/p/ABC123", "/alice", "/stories/alice/123"} {
		for _, tc := range []struct {
			ua, requestID, header string
			component, support    bool
		}{
			{"Discordbot/2.0", "2", "component", true, false},
			{"Discordbot/2.0", "15", "component", true, true},
			{"Discordbot/2.0", "0", "mastodon", false, false},
			{"TelegramBot", "36", "", false, false},
			{"OGInstagramPreviewBot/1.0", "36", "", false, false},
			{"", "36", "", false, false},
		} {
			r := publicRequest("GET", path)
			r.Header.Set("User-Agent", tc.ua)
			r.Header.Set("Cf-Ray", tc.requestID)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `id="discord:component-embed"`) != tc.component {
				t.Errorf("component negotiation for %s with %+v returned %d: %s", path, tc, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), `type="application/activity+json"`) == tc.component {
				t.Errorf("ActivityPub discovery negotiation for %s with %+v is incorrect", path, tc)
			}
			if w.Header().Get("X-OGInstagram-Embed") != tc.header || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("variant/cache response headers for %s with %+v: %v", path, tc, w.Header())
			}
			if strings.Contains(w.Body.String(), "https://ko-fi.com/seirenkr") != tc.support {
				t.Errorf("support button negotiation for %s with %+v is incorrect", path, tc)
			}
		}
	}
	for _, path := range []string{"/p/ABC123/2", "/p/ABC123?img_index=2"} {
		r := publicRequest("GET", path)
		r.Header.Set("User-Agent", "Discordbot")
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, "2"))
		result := serveApp(t, g.app, r)
		paths := componentMediaPaths(t, componentPayload(t, string(result.body)), g.app.offloadSigner)
		if len(paths) != 1 || paths[0] != "/offload/ABC123/2" {
			t.Errorf("component URL selection for %s: %v", path, paths)
		}
	}
	for _, path := range []string{"/p/ABC123", "/stories/alice/123"} {
		r := publicRequest("GET", path)
		r.Host = "d.oginstagram.com"
		r.Header.Set("User-Agent", "Discordbot")
		result := serveApp(t, g.app, r)
		if result.status != http.StatusFound || result.headers["Location"] != post.Attachments[0].URL || len(result.body) != 0 || result.headers["X-OGInstagram-Embed"] != "" {
			t.Errorf("direct route changed for %s: %+v", path, result)
		}
	}
	r := publicRequest("GET", "/alice")
	r.Host = "d.oginstagram.com"
	r.Header.Set("User-Agent", "Discordbot")
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, "2"))
	componentPayload(t, string(serveApp(t, g.app, r).body))
	r = publicRequest("GET", "/p/GONE12")
	r.Header.Set("User-Agent", "Discordbot")
	if result := serveApp(t, g.app, r); result.status != http.StatusOK || strings.Contains(string(result.body), `id="discord:component-embed"`) {
		t.Error("error card should remain a legacy HTML embed")
	}
	r = publicRequest("HEAD", "/p/ABC123")
	r.Header.Set("User-Agent", "Discordbot")
	r.Header.Set("Cf-Ray", "36")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("X-OGInstagram-Embed") != "component" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("HEAD returned a body: status=%d length=%d", w.Code, w.Body.Len())
	}
	r = publicRequest("POST", "/api/embed")
	r.Body = io.NopCloser(strings.NewReader(`{"path":"/p/ABC123"}`))
	r.Header.Set("User-Agent", "Discordbot")
	r.Header.Set("Cf-Ray", "36")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://oginstagram.com")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Cookie", "cf_clearance=component-test")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/html") || strings.Contains(w.Body.String(), `id="discord:component-embed"`) || !strings.Contains(w.Body.String(), `type="application/activity+json"`) || w.Header().Get("X-OGInstagram-Embed") != "" || strings.Contains(w.Body.String(), "https://ko-fi.com/seirenkr") {
		t.Errorf("frontend preview must remain legacy HTML regardless of caller UA: %d %s", w.Code, w.Body.String())
	}
	status := statusSnowcode("p", post.Shortcode, 0, false, false)
	for _, path := range []string{"/api/v1/statuses/" + status, "/users/alice/statuses/" + status} {
		r = publicRequest("GET", path)
		r.Header.Set("User-Agent", "Discordbot")
		r.Header.Set("Cf-Ray", "2")
		w = httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "json") || !json.Valid(w.Body.Bytes()) || w.Header().Get("X-OGInstagram-Embed") != "" {
			t.Errorf("Component rollout changed status API %s: %d %v", path, w.Code, w.Header())
		}
	}
	signed, err := url.Parse(g.app.offloadURL("https://oginstagram.com", post.Shortcode, 0, false))
	if err != nil {
		t.Fatal(err)
	}
	r = publicRequest("GET", signed.RequestURI())
	r.Header.Set("User-Agent", "Discordbot")
	r.Header.Set("Cf-Ray", "2")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != post.Attachments[0].URL || w.Header().Get("X-OGInstagram-Embed") != "" {
		t.Errorf("Component rollout changed offload delivery: %d %v", w.Code, w.Header())
	}
}

func TestGatewayComponentRolloutSharesTheModelCache(t *testing.T) {
	g := testGateway(t)
	post := componentTestPost()
	g.app.posts.storeLocal(post.Shortcode, &cacheEntry[Post]{value: post, expiresAt: time.Now().Add(time.Hour)})
	counts := map[string]int{}
	const requests = 2048
	supportCount := 0
	for i := range requests {
		r := publicRequest("GET", "/p/ABC123")
		r.Header.Set("User-Agent", "Discordbot/2.0")
		r.Header.Set("Cf-Ray", fmt.Sprintf("rollout-test-%03d", i))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("cached model rollout response %d: %d %v", i, w.Code, w.Header())
		}
		variant := w.Header().Get("X-OGInstagram-Embed")
		component := strings.Contains(w.Body.String(), `id="discord:component-embed"`)
		activity := strings.Contains(w.Body.String(), `type="application/activity+json"`)
		if (variant != "component" && variant != "mastodon") || component == activity || component != (variant == "component") {
			t.Fatalf("cached model served inconsistent variant %q: component=%v activity=%v", variant, component, activity)
		}
		counts[variant]++
		if component {
			for _, button := range componentsOfType(componentPayload(t, w.Body.String()), 2) {
				if button["url"] == "https://ko-fi.com/seirenkr" {
					supportCount++
				}
			}
		} else if strings.Contains(w.Body.String(), "https://ko-fi.com/seirenkr") {
			t.Fatal("legacy rollout request contained a support button")
		}
	}
	// Fixed request ids make the population repeatable while exercising the real
	// gateway and shared cached model. A sticky per-post assignment fails here.
	for _, variant := range []string{"component", "mastodon"} {
		if counts[variant] < requests*45/100 || counts[variant] > requests*55/100 {
			t.Errorf("%d independent request ids did not yield a roughly even rollout: %v", requests, counts)
		}
	}
	if supportCount < counts["component"]*14/100 || supportCount > counts["component"]*26/100 {
		t.Errorf("support should appear on about 20%% of component requests, got %d of %d", supportCount, counts["component"])
	}
}

func TestDiscordEmbedResponseReportsActualFallback(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	post := componentTestPost()
	r := publicRequest("GET", "/p/ABC123")
	r.Header.Set("User-Agent", "Discordbot")
	for _, tc := range []struct {
		document, want string
	}{
		{a.buildEmbedHTML("https://oginstagram.com/"+strings.Repeat("x", 1500), post, "p", 0, false, false, discordComponents), "mastodon"},
		{a.buildStatusEmbedHTML("https://oginstagram.com", "https://www.instagram.com/p/ABC123/", "Unavailable", "Try again later"), "opengraph"},
	} {
		response := discordEmbedResponse(r, tc.document, discordComponents)
		if response.status != http.StatusOK || response.headers["X-OGInstagram-Embed"] != tc.want || strings.Contains(string(response.body), `id="discord:component-embed"`) {
			t.Errorf("component assignment should report its actual %s fallback: %+v", tc.want, response)
		}
	}
}

func TestDiscordEmbedQueryOverride(t *testing.T) {
	discord := func(query string) *http.Request {
		req := httptest.NewRequest("GET", "/p/ABC123?"+query, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)")
		return req
	}
	for query, want := range map[string]discordEmbedVariant{
		"e=c": discordComponents, "e=c&n=2": discordComponents, "img_index=2&e=c": discordComponents,
		"e=s": discordComponentsSupport, "e=s&n=2": discordComponentsSupport,
	} {
		for i := 0; i < 20; i++ {
			if got := discordRequestVariant(discord(query)); got != want {
				t.Fatalf("%s: variant %d, want %d", query, got, want)
			}
		}
	}
	// Other values keep the normal split, so both arms still appear.
	for _, query := range []string{"ce", "e=component", "e=C", "e=S"} {
		seen := map[discordEmbedVariant]bool{}
		for i := 0; i < 200; i++ {
			seen[discordRequestVariant(discord(query))] = true
		}
		if !seen[discordLegacy] || !seen[discordComponents] {
			t.Errorf("%s must not pin a layout: %v", query, seen)
		}
	}
	req := httptest.NewRequest("GET", "/p/ABC123?e=c", nil)
	req.Header.Set("User-Agent", "TelegramBot (like TwitterBot)")
	if discordRequestVariant(req) != discordLegacy {
		t.Error("the override must not give non-Discord clients a Component Embed")
	}
}
