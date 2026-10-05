package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testOffloadSigningKeys = `{"active":"test-v1","keys":{"test-v1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`

const (
	testOffloadFutureExpiry  = int64(4102444800)
	testOffloadExpiredExpiry = int64(1000000000)
)

func mustOffloadSigner(raw string) offloadSigner {
	signer, err := parseOffloadSigner(raw)
	if err != nil {
		panic(err)
	}
	return signer
}

func TestOffloadSignerMatchesSharedHMACVector(t *testing.T) {
	signer, err := parseOffloadSigner(testOffloadSigningKeys)
	if err != nil {
		t.Fatal(err)
	}
	const (
		path      = "/offload/Ab_12/1"
		signature = "CqgnG0rUpAoYVk7_SysTiO6WSXKBb4syu-WbZPuYDtY"
	)
	if got := signer.signature(path, testOffloadFutureExpiry); got != signature {
		t.Fatalf("signature = %q, want shared vector %q", got, signature)
	}
	for _, tampered := range []string{
		"/offload/Ab_12/2",
		"/offload/ab_12/1",
		"/offload/Ab_12/1/avatar",
	} {
		if signer.signature(tampered, testOffloadFutureExpiry) == signature {
			t.Errorf("path %q produced the same signature", tampered)
		}
	}
	if signer.signature(path, testOffloadFutureExpiry+1) == signature {
		t.Error("expiry is not bound into the signature")
	}
	if expired := signer.signature(path, testOffloadExpiredExpiry); expired != "ImT53jUaC5umQORFyO5QWo2lpCHoAA_DNHVFWUD1xts" {
		t.Fatalf("expired vector = %q", expired)
	}
}

func TestOffloadURLsAreCanonicalAndResourceScoped(t *testing.T) {
	a := &App{offloadSigner: mustOffloadSigner(testOffloadSigningKeys)}
	urls := []struct {
		raw  string
		path string
	}{
		{a.offloadURL("https://oginstagram.com/", "Ab_12", 0, true), "/offload/Ab_12/1"},
		{a.postAvatarURL("https://oginstagram.com", Post{Shortcode: "Ab_12", ProfilePic: "x"}), "/offload/Ab_12/avatar"},
		{a.profileAvatarURL("https://oginstagram.com", Profile{Username: "User.Name", ProfilePic: "x"}), "/offload/@user.name/avatar"},
		{a.profileMediaOffloadURL("https://oginstagram.com", "User.Name", 1), "/offload/@user.name/2"},
		{a.storyOffloadURL("https://oginstagram.com", "User.Name", "123", false), "/offload/story/user.name/123"},
		{a.storyAvatarURL("https://oginstagram.com", Story{Username: "User.Name", ID: "123", ProfilePic: "x"}), "/offload/story/user.name/123/avatar"},
	}
	for _, tc := range urls {
		parsed, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if parsed.Path != tc.path {
			t.Errorf("path = %q, want %q", parsed.Path, tc.path)
		}
		if !a.offloadSigner.authorize(parsed, time.Now()) {
			t.Errorf("generated capability is not authorized: %s", tc.raw)
		}
		expires, ok := parseCanonicalDecimal(parsed.Query().Get("exp"))
		if lifetime := time.Until(time.Unix(int64(expires), 0)); !ok ||
			lifetime > offloadCapabilityTTL || lifetime < offloadCapabilityTTL-time.Minute {
			t.Errorf("capability lifetime = %v, want ~%v: %s", lifetime, offloadCapabilityTTL, tc.raw)
		}
	}
	if got := urls[0].raw; !strings.Contains(got, "?thumbnail=1&v=2&kid=test-v1&exp=") {
		t.Errorf("thumbnail URL query = %q, want canonical representation then capability", got)
	}
}

func TestOffloadSignerSignsWithTheRotatedKey(t *testing.T) {
	oldSigner := mustOffloadSigner(testOffloadSigningKeys)
	oldSignature := oldSigner.signature("/offload/Ab_12/1", testOffloadFutureExpiry)
	newKey := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("\x01", 32)))
	rotated := mustOffloadSigner(fmt.Sprintf(
		`{"active":"test-v2","keys":{"test-v1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","test-v2":%q}}`,
		newKey,
	))
	if rotated.signature("/offload/Ab_12/1", testOffloadFutureExpiry) == oldSignature {
		t.Fatal("rotated keyring still signed with the old key")
	}
	// Sign before reading the clock so a second boundary cannot push exp past the TTL.
	old, _ := url.Parse(oldSigner.url("https://x", "/offload/Ab_12/1", false))
	now := time.Now()
	if !rotated.authorize(old, now) {
		t.Error("rotated keyring rejected a capability signed with the retained key")
	}
	if mustOffloadSigner(fmt.Sprintf(`{"active":"test-v2","keys":{"test-v2":%q}}`, newKey)).authorize(old, now) {
		t.Error("capability signed with a removed key was authorized")
	}
}

func TestOffloadAuthorizeRejectsQueryMutations(t *testing.T) {
	signer := mustOffloadSigner(testOffloadSigningKeys)
	const path = "/offload/Ab_12/1"
	signed := signer.url("https://x", path, false) // before now: see the rotation test
	now := time.Now()
	resign := func(q url.Values, expires int64) {
		q.Set("exp", strconv.FormatInt(expires, 10))
		q.Set("sig", signer.signature(path, expires))
	}
	ttl := int64(offloadCapabilityTTL / time.Second)
	for _, tt := range []struct {
		name   string
		mutate func(url.Values)
		want   bool
	}{
		{"unchanged", func(url.Values) {}, true},
		{"v missing", func(q url.Values) { q.Del("v") }, false},
		{"duplicate sig", func(q url.Values) { q.Add("sig", q.Get("sig")) }, false},
		{"v=3", func(q url.Values) { q.Set("v", "3") }, false},
		{"unknown kid", func(q url.Values) { q.Set("kid", "nope") }, false},
		{"expired", func(q url.Values) { resign(q, now.Unix()-1) }, false},
		{"beyond TTL", func(q url.Values) { resign(q, now.Unix()+ttl+1) }, false},
		{"non-canonical exp", func(q url.Values) { q.Set("exp", "0"+q.Get("exp")) }, false},
		{"padded sig", func(q url.Values) { q.Set("sig", q.Get("sig")+"=") }, false},
	} {
		u, _ := url.Parse(signed)
		q := u.Query()
		tt.mutate(q)
		u.RawQuery = q.Encode()
		if got := signer.authorize(u, now); got != tt.want {
			t.Errorf("%s: authorize = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestOffloadSignerRejectsMalformedKeyrings(t *testing.T) {
	for _, raw := range []string{
		"",
		`{}`,
		`{"active":"missing","keys":{"other":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`,
		`{"active":"bad.id","keys":{"bad.id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`,
		`{"active":"test","keys":{"test":"AA"}}`,
		`{"active":"test","keys":{"test":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}`,
		`{"active":"test","keys":{"test":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB"}}`,
		`{"active":"test","keys":{"test":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},"extra":true}`,
		testOffloadSigningKeys + `{}`,
	} {
		if _, err := parseOffloadSigner(raw); err == nil {
			t.Errorf("parseOffloadSigner(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestOffloadSignerNeverEmitsUnsignedURL(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("zero-value signer emitted an unsigned URL")
		}
	}()
	_ = (offloadSigner{}).url("https://oginstagram.com", "/offload/Ab_12/1", false)
}
