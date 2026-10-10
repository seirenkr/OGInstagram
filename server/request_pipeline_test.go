package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andybalholm/brotli"
)

func TestPausedEmbedFailureIsCached(t *testing.T) {
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	var helpers atomic.Int32
	externalHelperPostImpl = func(_ *App, _ context.Context, _ string) (Post, bool) { helpers.Add(1); return Post{}, false }
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "get_ruling_for_content") {
			t.Error("paused embed unexpectedly fetched")
		}
		return mediaTestResponse(r, 200, nil, []byte(`{"status":"ok"}`)), nil
	}))
	synctest.Test(t, func(t *testing.T) {
		var gql, oembed atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/api/v1/oembed/" {
				oembed.Add(1)
				return mediaTestResponse(r, 200, nil, []byte(`{"status":"fail"}`)), nil
			}
			gql.Add(1)
			return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}, budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(2 * time.Minute).UnixNano())
		for n := 0; n < 2; n++ {
			_, err := a.getPost(context.Background(), "ABC123", nil)
			if err == nil || err.Ephemeral || err.Code != errorCodeGraphQL || err.Status == 404 {
				t.Fatalf("final failure = %#v; want cacheable transient GraphQL error", err)
			}
		}
		entry, ok := a.posts.localGet("ABC123")
		if !ok || time.Until(entry.expiresAt) != transientErrorCacheSeconds*time.Second {
			t.Fatal("transient failure should have the short error TTL")
		}
		if gql.Load() != 1 || oembed.Load() != 1 || helpers.Load() != 1 {
			t.Fatalf("duplicate calls: GraphQL=%d oEmbed=%d helper=%d", gql.Load(), oembed.Load(), helpers.Load())
		}
		time.Sleep(transientErrorCacheSeconds * time.Second)
		_, _ = a.getPost(context.Background(), "ABC123", nil)
		if gql.Load() != 2 || oembed.Load() != 2 || helpers.Load() != 2 {
			t.Fatal("transient failure did not retry after the short TTL")
		}
	})
}

func TestSlowRulingRetainsOneHelperAndFillsCache(t *testing.T) {
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	var helpers atomic.Int32
	externalHelperPostImpl = func(_ *App, ctx context.Context, key string) (Post, bool) {
		helpers.Add(1)
		select {
		case <-time.After(1500 * time.Millisecond):
			return Post{Shortcode: key, Username: "instagram", Attachments: []Attachment{{Kind: "image", URL: "https://scontent.cdninstagram.com/x.jpg"}}}, true
		case <-ctx.Done():
			return Post{}, false
		}
	}
	stubDefaultTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mediaTestResponse(r, 404, nil, []byte(`{"status":"fail","message":"Media cannot be found"}`)), nil
	}))
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			time.Sleep(2100 * time.Millisecond)
			return mediaTestResponse(r, 200, nil, []byte(`{"data":null,"errors":[{"message":"field_exception","code":1675030}]}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}, budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(time.Minute).UnixNano())
		started := time.Now()
		_, err := a.getPost(context.Background(), "ABC123", nil)
		if err == nil || !err.Final || time.Since(started) != 2100*time.Millisecond {
			t.Fatal("missing verdict should return without waiting for helper")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		entry, ok := a.posts.localGet("ABC123")
		if helpers.Load() != 1 || !ok || entry.err != nil || entry.value.Username != "instagram" {
			t.Fatal("existing helper should continue once and replace the missing verdict")
		}
	})
}

func TestHelperCancelledWhenGraphQLWins(t *testing.T) {
	previous := externalHelperPostImpl
	t.Cleanup(func() { externalHelperPostImpl = previous })
	helperCancelled := make(chan struct{})
	externalHelperPostImpl = func(_ *App, ctx context.Context, _ string) (Post, bool) {
		<-ctx.Done()
		close(helperCancelled)
		return Post{}, false
	}
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			time.Sleep(2100 * time.Millisecond)
			return mediaTestResponse(r, 200, nil, []byte(`{"data":{"xdt_api__v1__media__shortcode__web_info":{"items":[{"code":"ABC123","user":{"username":"instagram"},"display_url":"https://cdn.example/image.jpg","media_type":1}]}}}`)), nil
		})}
		pool := &SessionPool{sessions: []*Session{{client: client}}, budgetLeaseExpires: time.Now().Add(time.Hour), budgetLeaseRemaining: 1 << 20}
		a := newApp(Config{}, pool, offloadSigner{})
		a.embedPausedUntil.Store(time.Now().Add(time.Minute).UnixNano())
		if _, err := a.getPost(context.Background(), "ABC123", nil); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-helperCancelled:
		default:
			t.Fatal("losing helper kept consuming resources after a full result")
		}
	})
}

type unreadResponseBody struct {
	reads  int
	closed bool
}

func (b *unreadResponseBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *unreadResponseBody) Close() error             { b.closed = true; return nil }

func TestJSONRejectsHTMLAndRedirectWithoutReading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		kind   string
	}{
		{"html", 200, "text/html; charset=utf-8"},
		{"xhtml", 200, "application/xhtml+xml"},
		{"forbidden_html", 403, "text/html"},
		{"login_redirect", 302, "text/html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, redirects := 0, 0
			body := &unreadResponseBody{}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.kind}, "Location": []string{instagramOrigin + "/accounts/login/"}}, Body: body, Request: r}, nil
			})}
			status, _, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
			if status != tc.status || err == nil || calls != 1 || redirects != 0 || body.reads != 0 || !body.closed {
				t.Fatalf("status=%d err=%v calls=%d redirects=%d reads=%d closed=%t", status, err, calls, redirects, body.reads, body.closed)
			}
			if err := client.CheckRedirect(nil, nil); err != nil || redirects != 1 {
				t.Fatal("shared client's redirect policy was changed")
			}
		})
	}
}

func TestHTMLSourceStillFollowsRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return mediaTestResponse(r, 302, http.Header{"Location": []string{"https://example.com/page"}}, nil), nil
		}
		return mediaTestResponse(r, 200, http.Header{"Content-Type": []string{"text/html"}}, []byte("<html>page</html>")), nil
	})}
	_, body, err := fetchRequest(context.Background(), client, fetchSpec{method: http.MethodGet, url: "https://example.com/start", interpret: statusOnly})
	if err != nil || body != "<html>page</html>" || calls != 2 {
		t.Fatal("HTML source redirect/response behavior changed")
	}
}

func compressedResponse(t *testing.T, encoding string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	default:
		return raw
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchRequestCompression(t *testing.T) {
	const raw = `{"data":{"ok":true}}`
	for _, encoding := range []string{"", "identity", "gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			wire := compressedResponse(t, encoding, []byte(raw))
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Accept-Encoding") != "br, gzip" {
					t.Error("missing compression negotiation")
				}
				return mediaTestResponse(r, 200, http.Header{"Content-Encoding": []string{encoding}, "Content-Type": []string{"text/javascript; charset=utf-8"}}, wire), nil
			})}
			_, body, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
			if err != nil || body != raw {
				t.Fatalf("decoded response = %q, %v", body, err)
			}
		})
	}
	t.Run("already_decompressed", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp := mediaTestResponse(r, 200, nil, []byte(raw))
			resp.Uncompressed = true
			return resp, nil
		})}
		_, body, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
		if err != nil || body != raw {
			t.Fatal("already-decompressed response was decoded again")
		}
	})
}

func TestFetchRequestCompressedBodyLimits(t *testing.T) {
	for _, encoding := range []string{"gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			wire := compressedResponse(t, encoding, []byte(strings.Repeat("x", maxResponseBytes+1)))
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaTestResponse(r, 200, http.Header{"Content-Encoding": []string{encoding}}, wire), nil
			})}
			_, _, err := fetchRequest(context.Background(), client, webLoggedOutSpec("ABC123"))
			if err == nil || !strings.Contains(err.PublicMessage, "too large") {
				t.Fatal("decoded body limit was not enforced")
			}
		})
	}
}
