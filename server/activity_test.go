package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestUserAccountActorDoesNotFetchProfile(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram", Port: 8080}}
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
	if actor["followers"] != "https://oginstagram.com/users/instagram/followers" {
		t.Fatalf("actor followers = %v", actor["followers"])
	}
	if actor["following"] != "https://oginstagram.com/users/instagram/following" {
		t.Fatalf("actor following = %v", actor["following"])
	}
	if _, ok := actor["icon"]; ok {
		t.Fatalf("fallback actor should not include fetched profile icon: %#v", actor["icon"])
	}
}

func TestUserActivityCollectionsAreOrderedCollections(t *testing.T) {
	a := &App{cfg: Config{BrandName: "OGInstagram", Port: 8080}}
	req := httptest.NewRequest("GET", "https://oginstagram.com/users/instagram/outbox", nil)
	res := a.handleUserCollection(req, "instagram", "outbox")
	if res.status != 200 {
		t.Fatalf("status = %d, want 200", res.status)
	}

	var coll map[string]any
	if err := json.Unmarshal(res.body, &coll); err != nil {
		t.Fatal(err)
	}
	if coll["@context"] != asContext || coll["type"] != "OrderedCollection" {
		t.Fatalf("collection shape wrong: %#v", coll)
	}
	if coll["id"] != "https://oginstagram.com/users/instagram/outbox" {
		t.Fatalf("collection id = %v", coll["id"])
	}
	if coll["totalItems"] != float64(0) {
		t.Fatalf("totalItems = %#v, want 0", coll["totalItems"])
	}
	if items, ok := coll["orderedItems"].([]any); !ok || len(items) != 0 {
		t.Fatalf("orderedItems = %#v, want empty array", coll["orderedItems"])
	}
}
