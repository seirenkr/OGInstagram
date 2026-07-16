package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUserAccountActorDoesNotFetchProfile(t *testing.T) {
	a := &App{cfg: Config{Port: 8080}}
	req := httptest.NewRequest("GET", "https://oginstagram.com/users/instagram", nil)
	res := a.handleUserAccount(req, "instagram")
	if res.status != 200 {
		t.Fatalf("status = %d, want 200", res.status)
	}

	var actor map[string]any
	if err := json.Unmarshal(res.body, &actor); err != nil {
		t.Fatal(err)
	}
	if actor["name"] != "instagram" || actor["preferredUsername"] != "instagram" {
		t.Fatalf("actor should use username-only fallback, got %#v", actor)
	}
	if actor["url"] != "https://oginstagram.com/users/instagram" {
		t.Fatalf("actor url = %v, want local actor URL", actor["url"])
	}
	if actor["inbox"] != "https://oginstagram.com/users/instagram/inbox" {
		t.Fatalf("actor inbox = %v", actor["inbox"])
	}
	if actor["outbox"] != "https://oginstagram.com/users/instagram/outbox" {
		t.Fatalf("actor outbox = %v", actor["outbox"])
	}
	if _, ok := actor["icon"]; ok {
		t.Fatalf("fallback actor should not include fetched profile icon: %#v", actor["icon"])
	}
}

func TestActivityCollectionsAreEmptyAndReadOnly(t *testing.T) {
	a := &App{cfg: Config{BaseURL: "https://oginstagram.com"}}
	for _, name := range []string{"inbox", "outbox"} {
		req := httptest.NewRequest(http.MethodGet, "https://oginstagram.com/users/instagram/"+name, nil)
		res := a.handleActivityCollection(req, "instagram", name)
		var collection struct {
			Type         string `json:"type"`
			TotalItems   int    `json:"totalItems"`
			OrderedItems []any  `json:"orderedItems"`
		}
		if res.status != http.StatusOK || json.Unmarshal(res.body, &collection) != nil || collection.Type != "OrderedCollection" || collection.TotalItems != 0 || len(collection.OrderedItems) != 0 {
			t.Fatalf("%s collection = status %d, body %s", name, res.status, res.body)
		}
		post := httptest.NewRequest(http.MethodPost, req.URL.String(), nil)
		if got := a.handleActivityCollection(post, "instagram", name); got.status != http.StatusMethodNotAllowed || got.headers["Allow"] != "GET" {
			t.Fatalf("POST %s = %#v", name, got)
		}
	}
}

func TestWebFingerReturnsActorAndFiltersRelations(t *testing.T) {
	a := &App{cfg: Config{BaseURL: "https://oginstagram.com"}}
	req := httptest.NewRequest(http.MethodGet, "https://oginstagram.com/.well-known/webfinger?resource=acct%3Ainstagram%40oginstagram.com", nil)
	res := a.handleWebFinger(req)
	var body struct {
		Subject string `json:"subject"`
		Links   []struct {
			Rel  string `json:"rel"`
			Type string `json:"type"`
			Href string `json:"href"`
		} `json:"links"`
	}
	if res.status != http.StatusOK || res.headers["Content-Type"] != "application/jrd+json" || json.Unmarshal(res.body, &body) != nil {
		t.Fatalf("webfinger = status %d, headers %#v, body %s", res.status, res.headers, res.body)
	}
	if body.Subject != "acct:instagram@oginstagram.com" || len(body.Links) != 1 || body.Links[0].Href != "https://oginstagram.com/users/instagram" {
		t.Fatalf("webfinger body = %#v", body)
	}
	filtered := httptest.NewRequest(http.MethodGet, req.URL.String()+"&rel=unknown", nil)
	if got := a.handleWebFinger(filtered); !strings.Contains(string(got.body), `"links":[]`) {
		t.Fatalf("filtered webfinger body = %s", got.body)
	}
}

func TestActivityStatusLinkifiesCaptionAndCapsImages(t *testing.T) {
	post := Post{
		Shortcode: "CODE",
		Username:  "alice",
		Caption:   "hello @bob #sunset",
		Attachments: []Attachment{
			{Kind: "image", URL: "https://cdn.example/1.jpg"},
			{Kind: "image", URL: "https://cdn.example/2.jpg"},
			{Kind: "image", URL: "https://cdn.example/3.jpg"},
			{Kind: "image", URL: "https://cdn.example/4.jpg"},
			{Kind: "image", URL: "https://cdn.example/5.jpg"},
		},
	}
	var note struct {
		Content    string            `json:"content"`
		Attachment []json.RawMessage `json:"attachment"`
	}
	if err := json.Unmarshal((&App{}).buildActivityStatus("https://oginstagram.com", post, "p", 0, false, false), &note); err != nil {
		t.Fatal(err)
	}
	if len(note.Attachment) != activityMaxImages {
		t.Fatalf("attachments = %d, want %d", len(note.Attachment), activityMaxImages)
	}
	for _, want := range []string{`href="https://www.instagram.com/bob"`, `q=%23sunset`} {
		if !strings.Contains(note.Content, want) {
			t.Fatalf("content missing %q: %s", want, note.Content)
		}
	}
}
