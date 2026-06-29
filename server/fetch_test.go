package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNewLSDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s := newLSD()
		if len(s) < 23 || len(s) > 27 {
			t.Fatalf("lsd length %d out of range [23,27]: %q", len(s), s)
		}
		for _, c := range s {
			ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !ok {
				t.Fatalf("lsd has non-alphanumeric char %q in %q", c, s)
			}
		}
		seen[s] = true
	}
	if len(seen) < 90 {
		t.Errorf("lsd not random enough: %d unique of 100", len(seen))
	}
}

func TestShortcodeToPK(t *testing.T) {

	if got := shortcodeToPK("DaEd82_pQ40"); got != "3928396500541181492" {
		t.Errorf("shortcodeToPK(DaEd82_pQ40) = %q, want 3928396500541181492", got)
	}
	if got := shortcodeToPK("has space"); got != "" {
		t.Errorf("invalid char should yield empty, got %q", got)
	}
}

func TestWebLoggedOutSpec(t *testing.T) {
	spec := webLoggedOutSpec("DaEd82_pQ40")
	if spec.method != http.MethodPost {
		t.Errorf("method = %q, want POST", spec.method)
	}
	if spec.url != "https://www.instagram.com/api/graphql" {
		t.Errorf("url = %q", spec.url)
	}
	if spec.headers["X-FB-Friendly-Name"] != "PolarisLoggedOutDesktopWWWPostRootContentQuery" {
		t.Errorf("friendly name = %q", spec.headers["X-FB-Friendly-Name"])
	}
	if spec.headers["X-Requested-With"] != "XMLHttpRequest" {
		t.Errorf("x-requested-with = %q", spec.headers["X-Requested-With"])
	}
	vals, err := url.ParseQuery(spec.body)
	if err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if vals.Get("doc_id") != instagramWebLoggedOutDocID {
		t.Errorf("doc_id = %q, want %q", vals.Get("doc_id"), instagramWebLoggedOutDocID)
	}
	if lsd := vals.Get("lsd"); lsd == "" || lsd != spec.headers["X-FB-LSD"] {
		t.Errorf("lsd mismatch: body=%q header=%q", lsd, spec.headers["X-FB-LSD"])
	}

	v := vals.Get("variables")
	if !strings.Contains(v, `"media_id":"3928396500541181492"`) || strings.Contains(v, "DaEd82_pQ40") {
		t.Errorf("variables should hold decoded media_id, got %q", v)
	}
}
