package main

import (
	"encoding/json"
	"strings"
	"testing"
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
		`property="twitter:description" content=""`,
	} {
		if !strings.Contains(gallery, tag) {
			t.Errorf("gallery embed is missing %s", tag)
		}
	}
	if strings.Contains(gallery, `property="og:image:alt"`) || strings.Contains(gallery, `property="twitter:image:alt"`) {
		t.Error("gallery embed must not expose the caption through image alt metadata")
	}
	galleryCode := activityCodeFor("p", "CODE", 0, false, true)
	if !strings.Contains(gallery, `/users/user/statuses/`+galleryCode) {
		t.Error("gallery embed does not preserve gallery mode in its ActivityPub link")
	}
	if galleryCode == activityCodeFor("p", "CODE", 0, false, false) {
		t.Error("gallery and normal ActivityPub IDs must differ")
	}
	if route := parseActivityCode(galleryCode); !route.Gallery || route.Shortcode != "CODE" {
		t.Errorf("gallery ActivityPub ID decoded incorrectly: %#v", route)
	}

	var status map[string]any
	body := a.buildActivityStatus("https://oginstagram.com", "status", post, "p", 0, false, true)
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if status["content"] != "" {
		t.Errorf("gallery ActivityPub content = %q, want empty", status["content"])
	}
	if media, ok := status["media_attachments"].([]any); !ok || len(media) != 1 {
		t.Errorf("gallery ActivityPub media_attachments = %#v, want one item", status["media_attachments"])
	}
}
