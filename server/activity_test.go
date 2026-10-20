package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	asProfileAccept  = `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
	mastodonAccept   = `application/activity+json, application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
	browserAccept    = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	apTestActor      = "https://oginstagram.com/alice"
	apTestGalleryURL = "https://g.oginstagram.com"
)

func apTestGateway(t *testing.T) *Gateway {
	t.Helper()
	g := testGateway(t)
	g.cfg.AllowedHosts = append(g.cfg.AllowedHosts, "www.oginstagram.com")
	exp := time.Now().Add(time.Hour)
	cdn := "https://scontent.cdninstagram.com/"
	var media []Attachment
	for _, n := range []string{"1", "2", "3"} {
		media = append(media, Attachment{Kind: "image", URL: cdn + n + ".jpg", Thumbnail: cdn + n + ".jpg", Width: 1080, Height: 1080})
	}
	g.app.posts.storeLocal("ABC123", &cacheEntry[Post]{value: Post{Shortcode: "ABC123", Username: "alice", Caption: "caption", Attachments: media}, expiresAt: exp})
	g.app.profiles.storeLocal("alice", &cacheEntry[Profile]{value: Profile{Username: "alice", FullName: "Alice", Biography: "bio", ProfilePic: cdn + "a.jpg"}, expiresAt: exp})
	g.app.stories.storeLocal("alice/123", &cacheEntry[Story]{value: Story{ID: "123", Username: "alice", Caption: "story", Media: media[0]}, expiresAt: exp})
	return g
}

func apGet(g *Gateway, method, rawURL, accept string) *httptest.ResponseRecorder {
	u, _ := url.Parse(rawURL)
	r := publicRequest(method, u.RequestURI())
	r.Host = u.Host
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestActivityContentNegotiation(t *testing.T) {
	g := apTestGateway(t)
	activityAccepts := []string{
		asMediaType, asProfileAccept, "application/ld+json", mastodonAccept,
		// Mastodon's URL lookup, then the SWICG discovery example.
		`application/ld+json; profile="https://www.w3.org/ns/activitystreams", application/activity+json, text/html;q=0.1`,
		"application/activity+json;q=1.0, application/ld+json;q=0.8, application/json;q=0.5",
	}
	htmlAccepts := []string{browserAccept, "*/*", "", "application/json", "text/html, application/activity+json;q=0.5", "application/activity+json;q=0"}
	for _, tc := range []struct{ url, id, actor, kind string }{
		{"https://oginstagram.com/p/ABC123", "https://oginstagram.com/p/ABC123", apTestActor, "Note"},
		{"https://oginstagram.com/alice/reels/ABC123/?img_index=2&e=s&n=7", "https://oginstagram.com/reel/ABC123/2", apTestActor, "Note"},
		{apTestGalleryURL + "/p/ABC123/99", apTestGalleryURL + "/p/ABC123/3", apTestGalleryURL + "/alice", "Note"},
		{"https://www.oginstagram.com/p/ABC123?index=0", "https://oginstagram.com/p/ABC123/1", apTestActor, "Note"},
		{"https://oginstagram.com/stories/Alice/123", "https://oginstagram.com/stories/alice/123", apTestActor, "Note"},
		{"https://oginstagram.com/Alice", apTestActor, "", "Person"},
	} {
		for _, accept := range activityAccepts {
			w := apGet(g, http.MethodGet, tc.url, accept)
			wantLink := `<` + tc.id + `>; rel="alternate"; type="text/html"`
			if tc.actor != "" {
				wantLink += `, <` + tc.actor + `>; rel="author"; type="application/activity+json"`
			}
			var object map[string]any
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != asMediaType || w.Header().Get("Vary") != "Accept" ||
				w.Header().Get("Cloudflare-CDN-Cache-Control") != "" || w.Header().Get("Link") != wantLink || json.Unmarshal(w.Body.Bytes(), &object) != nil {
				t.Fatalf("%s with %q = %d %v %s", tc.url, accept, w.Code, w.Header(), w.Body.String())
			}
			link, _ := object["url"].(map[string]any)
			if object["@context"] != asContext || object["id"] != tc.id || object["type"] != tc.kind ||
				link["type"] != "Link" || link["href"] != tc.id || link["mediaType"] != "text/html" {
				t.Fatalf("%s with %q: %s", tc.url, accept, w.Body.String())
			}
			if tc.kind == "Note" && object["attributedTo"] != tc.actor {
				t.Errorf("%s: attributedTo = %v, want %s", tc.url, object["attributedTo"], tc.actor)
			}
			if tc.kind == "Person" {
				for _, name := range []string{"inbox", "outbox", "followers", "following"} {
					if object[name] != tc.id+"/"+name {
						t.Errorf("%s: %s = %v", tc.url, name, object[name])
					}
				}
				if object["preferredUsername"] != "alice" || object["name"] != "Alice" || object["summary"] == nil || object["icon"] == nil {
					t.Errorf("Person fields: %s", w.Body.String())
				}
			}
		}
		// Mastodon refetches by id unless the id dereferences to itself.
		if w := apGet(g, http.MethodGet, tc.id, mastodonAccept); !strings.Contains(w.Body.String(), `"id":"`+tc.id+`"`) {
			t.Errorf("%s does not dereference to itself: %d %s", tc.id, w.Code, w.Body.String())
		}
		for _, accept := range htmlAccepts {
			w := apGet(g, http.MethodGet, tc.url, accept)
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Vary") != "Accept" ||
				w.Header().Get("Link") != apLinks(tc.id, asMediaType, tc.actor) || !strings.Contains(body, strings.Join(apLinkTags(tc.id, tc.actor), "")) {
				t.Fatalf("%s with %q = %d %v %s", tc.url, accept, w.Code, w.Header(), body)
			}
			// Profiles never reach the edge cache; post and story cards do.
			if cached := w.Header().Get("Cloudflare-CDN-Cache-Control") != ""; cached != (tc.kind == "Note") {
				t.Errorf("%s with %q: Cloudflare-CDN-Cache-Control %q", tc.url, accept, w.Header().Get("Cloudflare-CDN-Cache-Control"))
			}
		}
	}
	// The preview renders the card whatever the caller accepts.
	r := publicRequest("POST", "/api/embed")
	r.Header.Set("Accept", asMediaType)
	if internal, _, ok := previewTarget(r, "/p/ABC123"); !ok || wantsActivity(internal) {
		t.Error("preview target kept the caller's AS2 Accept")
	}
}

func TestWantsActivity(t *testing.T) {
	for accept, want := range map[string]bool{
		"Application/Activity+JSON":                      true,
		"application/activity+json, text/html":           true,
		"text/*;q=0.5, application/ld+json;q=0.4":        false,
		"text/html;q=0.4, */*, application/ld+json;q=.5": true,
		"application/*":                    false,
		"application/activity+json;q=nope": false,
	} {
		r := httptest.NewRequest(http.MethodGet, "/p/ABC123", nil)
		r.Header.Set("Accept", accept)
		if got := wantsActivity(r); got != want {
			t.Errorf("wantsActivity(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestActivityErrorsAreProblemJSON(t *testing.T) {
	g := apTestGateway(t)
	exp := time.Now().Add(time.Hour)
	g.app.posts.storeLocal("GONE12", &cacheEntry[Post]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: exp})
	g.app.posts.storeLocal("RSTR12", &cacheEntry[Post]{err: igErr(403, errorCodeRestricted, "restricted detail"), expiresAt: exp})
	g.app.posts.storeLocal("GEO123", &cacheEntry[Post]{err: igErr(451, errorCodeGeoBlocked, "geo detail"), expiresAt: exp})
	g.app.posts.storeLocal("DOWN12", &cacheEntry[Post]{err: igErr(502, errorCodeUpstream, "external helper had no post"), expiresAt: exp})
	g.app.stories.storeLocal("alice/999", &cacheEntry[Story]{err: igErr(404, errorCodeMediaNotFound, "story gone"), expiresAt: exp})
	g.app.profiles.storeLocal("bob", &cacheEntry[Profile]{err: igErr(404, errorCodeNotFound, "gone"), expiresAt: exp})
	for _, tt := range []struct {
		path, retry string
		status      int
	}{
		{"/p/GONE12", "", 404},
		{"/p/RSTR12/2", "", 451},
		{"/reel/GEO123", "", 451},
		{"/p/DOWN12", "60", 503},
		{"/stories/alice/999", "", 404},
		{"/bob", "", 404},
	} {
		w := apGet(g, http.MethodGet, "https://oginstagram.com"+tt.path, mastodonAccept)
		var problem struct{ Status int }
		if w.Code != tt.status || w.Header().Get("Content-Type") != "application/problem+json" || w.Header().Get("Retry-After") != tt.retry ||
			w.Header().Get("Vary") != "Accept" || w.Header().Get("Cloudflare-CDN-Cache-Control") != "" ||
			json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Status != tt.status || strings.Contains(w.Body.String(), " detail") || strings.Contains(w.Body.String(), "helper") {
			t.Errorf("%s = %d %v %s", tt.path, w.Code, w.Header(), w.Body.String())
		}
		// The HTML error card stays a plain 200 without discovery links.
		w = apGet(g, http.MethodGet, "https://oginstagram.com"+tt.path, "")
		if w.Code != http.StatusOK || w.Header().Get("Link") != "" || strings.Contains(w.Body.String(), "activity+json") {
			t.Errorf("%s error card = %d %v", tt.path, w.Code, w.Header())
		}
	}
}

func TestActivityCollectionsAndRemovedRoutes(t *testing.T) {
	g := apTestGateway(t)
	for _, base := range []string{"https://oginstagram.com", apTestGalleryURL} {
		for _, name := range []string{"inbox", "outbox", "followers", "following"} {
			id := base + "/alice/" + name
			w := apGet(g, http.MethodGet, id, mastodonAccept)
			var collection struct {
				ID           string `json:"id"`
				Type         string `json:"type"`
				AttributedTo string `json:"attributedTo"`
				TotalItems   int    `json:"totalItems"`
				OrderedItems []any  `json:"orderedItems"`
			}
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != asMediaType || json.Unmarshal(w.Body.Bytes(), &collection) != nil ||
				collection.ID != id || collection.Type != "OrderedCollection" || collection.AttributedTo != base+"/alice" || collection.TotalItems != 0 || len(collection.OrderedItems) != 0 {
				t.Errorf("%s = %d %s", id, w.Code, w.Body.String())
			}
		}
	}
	if w := apGet(g, http.MethodPost, "https://oginstagram.com/alice/inbox", asMediaType); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST inbox = %d", w.Code)
	}
	// Post routes win: /p/inbox is a post whose shortcode is "inbox".
	if route, ok := resolveGatewayRoute(publicRequest(http.MethodGet, "/p/inbox")); !ok || route.category != "posts" {
		t.Errorf("/p/inbox = %+v, %v", route, ok)
	}
	for _, path := range []string{"/users/alice", "/users/alice/inbox", "/users/alice/outbox", "/users/alice/statuses/123", "/api/v1/statuses/123"} {
		for _, accept := range []string{mastodonAccept, ""} {
			if w := apGet(g, http.MethodGet, "https://oginstagram.com"+path, accept); w.Code != http.StatusNotFound {
				t.Errorf("%s with %q = %d, want 404", path, accept, w.Code)
			}
		}
	}
}

func TestWebFinger(t *testing.T) {
	g := apTestGateway(t)
	type jrd struct {
		Subject string   `json:"subject"`
		Aliases []string `json:"aliases"`
		Links   []struct {
			Rel  string `json:"rel"`
			Type string `json:"type"`
			Href string `json:"href"`
		} `json:"links"`
	}
	for _, tt := range []struct {
		host, resource, subject, actor string
		status                         int
	}{
		{"oginstagram.com", "acct:Alice@OGInstagram.com", "acct:alice@oginstagram.com", apTestActor, 200},
		{"oginstagram.com", apTestActor, "acct:alice@oginstagram.com", apTestActor, 200},
		{"www.oginstagram.com", "acct:alice@oginstagram.com", "acct:alice@oginstagram.com", apTestActor, 200},
		{"g.oginstagram.com", "acct:alice@g.oginstagram.com", "acct:alice@g.oginstagram.com", apTestGalleryURL + "/alice", 200},
		{"oginstagram.com", "", "", "", 400},
		{"oginstagram.com", "instagram@oginstagram.com", "", "", 400},
		{"oginstagram.com", "acct:@oginstagram.com", "", "", 400},
		{"oginstagram.com", "acct:instagram@", "", "", 400},
		{"oginstagram.com", "acct:instagram@example.com", "", "", 404},
		{"oginstagram.com", "acct:bad-name@oginstagram.com", "", "", 404},
		{"oginstagram.com", "acct:alice@g.oginstagram.com", "", "", 404},
		{"oginstagram.com", "https://oginstagram.com/p/ABC123", "", "", 404},
		{"oginstagram.com", "https://example.com/alice", "", "", 404},
		{"oginstagram.com", "mailto:alice@oginstagram.com", "", "", 404},
	} {
		w := apGet(g, http.MethodGet, "https://"+tt.host+"/.well-known/webfinger?resource="+url.QueryEscape(tt.resource), "")
		var body jrd
		if w.Code != tt.status || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s %q = %d %v", tt.host, tt.resource, w.Code, w.Header())
			continue
		}
		if tt.status != 200 {
			continue
		}
		if w.Header().Get("Content-Type") != "application/jrd+json" || json.Unmarshal(w.Body.Bytes(), &body) != nil ||
			body.Subject != tt.subject || len(body.Aliases) != 1 || body.Aliases[0] != tt.actor || len(body.Links) != 3 {
			t.Fatalf("%s %q = %s", tt.host, tt.resource, w.Body.String())
		}
		for i, want := range [][2]string{{"self", asMediaType}, {"http://webfinger.net/rel/profile-page", "text/html"}, {"alternate", "text/html"}} {
			if l := body.Links[i]; l.Rel != want[0] || l.Type != want[1] || l.Href != tt.actor {
				t.Errorf("%q link %d = %+v", tt.resource, i, l)
			}
		}
	}
	w := apGet(g, http.MethodGet, "https://oginstagram.com/.well-known/webfinger?resource=acct:alice@oginstagram.com&rel=self&rel=unknown", "")
	var body jrd
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Links) != 1 || body.Links[0].Rel != "self" {
		t.Errorf("rel filter = %s", w.Body.String())
	}
	if w.Header().Get("Cloudflare-CDN-Cache-Control") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("WebFinger may not be edge-cached: %v", w.Header())
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
		Content    string `json:"content"`
		Attachment []struct {
			URL string `json:"url"`
		} `json:"attachment"`
	}
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	if err := json.Unmarshal(a.postNote("https://oginstagram.com", post, "p", 0, false, false), &note); err != nil {
		t.Fatal(err)
	}
	if len(note.Attachment) != activityMaxImages {
		t.Fatalf("attachments = %d, want %d", len(note.Attachment), activityMaxImages)
	}
	for _, att := range note.Attachment {
		u, err := url.Parse(att.URL)
		if err != nil || !strings.HasPrefix(u.Path, "/offload/CODE/") || !a.offloadSigner.authorize(u, time.Now()) {
			t.Fatalf("attachment url %q is not a signed offload URL", att.URL)
		}
	}
	for _, want := range []string{`href="https://www.instagram.com/bob"`, `q=%23sunset`} {
		if !strings.Contains(note.Content, want) {
			t.Fatalf("content missing %q: %s", want, note.Content)
		}
	}
}

func TestPostStatusContentOmitsEmptyStats(t *testing.T) {
	if got := postStatusContent(Post{Caption: "hi"}, "", false); got != "<p>hi</p>" {
		t.Fatalf("content = %q, want only the caption paragraph", got)
	}
}
