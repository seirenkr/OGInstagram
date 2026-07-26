package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
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

func signedCorrectly(s offloadSigner, path string, q url.Values) bool {
	expires, ok := parseCanonicalDecimal(q.Get("exp"))
	return ok && q.Get("v") == offloadSignatureVersion && q.Get("kid") == "test-v1" &&
		q.Get("sig") == s.signature(path, int64(expires))
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
		query := parsed.Query()
		if !signedCorrectly(a.offloadSigner, tc.path, query) {
			t.Errorf("generated capability is not signed correctly: %s", tc.raw)
		}
		expires, ok := parseCanonicalDecimal(query.Get("exp"))
		if lifetime := time.Until(time.Unix(int64(expires), 0)); !ok ||
			lifetime > 14*24*time.Hour-time.Minute ||
			lifetime < 14*24*time.Hour-time.Minute-2*time.Second {
			t.Errorf("capability lifetime = %v, want 14 days with a one-minute clock-skew margin: %s", lifetime, tc.raw)
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
