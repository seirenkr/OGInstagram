package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func stubMediaTransport(t *testing.T, rt http.RoundTripper) {
	prev := mediaHTTPClient.Transport
	mediaHTTPClient.Transport = rt
	t.Cleanup(func() { mediaHTTPClient.Transport = prev })
}

func mediaTestResponse(r *http.Request, status int, headers http.Header, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}

func testPreviewPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var body bytes.Buffer
	if err := png.Encode(&body, img); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func testMediaRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, "https://oginstagram.com"+path, nil)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("User-Agent", "Mozilla/5.0")
	return r
}

func TestTrustedMediaDestinations(t *testing.T) {
	for _, raw := range []string{"https://cdninstagram.com/media", "https://scontent.cdninstagram.com/media", "https://scontent.fbcdn.net/media", "https://FBCDN.NET./media"} {
		u, err := url.Parse(raw)
		if err != nil || !trustedMediaURL(u) {
			t.Errorf("trusted URL rejected: %s", raw)
		}
	}
	for _, raw := range []string{"http://cdninstagram.com/media", "https://cdninstagram.com:443/media", "https://user@fbcdn.net/media", "https://fbcdn.net.evil.example/media", "https://evilcdninstagram.com/media", "https://127.0.0.1/media", "https://metadata.google.internal/media"} {
		u, err := url.Parse(raw)
		if err == nil && trustedMediaURL(u) {
			t.Errorf("unsafe URL accepted: %s", raw)
		}
	}
}

func TestMediaRedirectsValidatedBeforeRequest(t *testing.T) {
	calls := 0
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaTestResponse(r, 302, http.Header{"Location": {"https://127.0.0.1/private"}}, nil), nil
	}))
	target, _ := url.Parse("https://cdninstagram.com/media")
	if _, err := loadMedia(context.Background(), target, http.MethodGet, make(http.Header)); err == nil || calls != 1 {
		t.Fatalf("unsafe redirect was followed: calls=%d error=%v", calls, err)
	}
	calls = 0
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaTestResponse(r, 302, http.Header{"Location": {"/next"}}, nil), nil
	}))
	if _, err := loadMedia(context.Background(), target, http.MethodGet, make(http.Header)); err == nil || calls != 4 {
		t.Fatalf("redirect cap was not enforced: calls=%d error=%v", calls, err)
	}
}

func TestMediaPreservesBotAndCrossOriginRedirects(t *testing.T) {
	calls := 0
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaTestResponse(r, 200, http.Header{"Content-Type": {"image/jpeg"}}, []byte("jpeg")), nil
	}))
	redirect := redirectResp("https://cdninstagram.com/media", 302)
	for _, kind := range []string{"bot", "cross-origin", "untrusted"} {
		r := testMediaRequest(http.MethodGet, "/offload/example")
		next := redirect
		switch kind {
		case "bot":
			r.Header.Set("User-Agent", "facebookexternalhit/1.1")
		case "cross-origin":
			r.Header.Set("Sec-Fetch-Site", "cross-site")
		case "untrusted":
			next = redirectResp("https://evil.example/media", 302)
		}
		if proxyOffloadMedia(httptest.NewRecorder(), r, next) {
			t.Errorf("%s redirect was intercepted", kind)
		}
	}
	if calls != 0 {
		t.Fatal("redirect-only requests fetched media")
	}
}

func TestMediaResizeAndCache(t *testing.T) {
	body := testPreviewPNG(t, 200, 100)
	calls := 0
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return mediaTestResponse(r, 200, http.Header{"Content-Type": {"image/png"}}, body), nil
	}))
	redirect := redirectResp("https://cdninstagram.com/resize-cache-test", 302)
	r := testMediaRequest(http.MethodGet, "/offload/example?preview=avatar")
	response := httptest.NewRecorder()
	if !proxyOffloadMedia(response, r, redirect) {
		t.Fatal("preview not handled")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(response.Body.Bytes()))
	if err != nil || config.Width != 96 || config.Height != 48 {
		t.Fatalf("unexpected preview: %+v %v", config, err)
	}
	if response.Header().Get("Content-Type") != "image/png" || response.Header().Get("Vary") != "Accept" || response.Header().Get("Cloudflare-CDN-Cache-Control") != "no-store" {
		t.Fatal("preview representation or signed cache policy incorrect")
	}
	head := httptest.NewRecorder()
	if !proxyOffloadMedia(head, testMediaRequest(http.MethodHead, "/offload/example?preview=avatar"), redirect) || head.Body.Len() != 0 || calls != 1 {
		t.Fatal("cached HEAD refetched or returned a body")
	}
	conditional := testMediaRequest(http.MethodGet, "/offload/example?preview=avatar")
	conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
	response = httptest.NewRecorder()
	proxyOffloadMedia(response, conditional, redirect)
	if response.Code != 304 || response.Body.Len() != 0 || calls != 1 {
		t.Fatal("cached preview conditional GET incorrect")
	}
}

func TestMediaRangeAndHeadAreForwarded(t *testing.T) {
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Fatal("origin credentials sent to media CDN")
		}
		if r.Method == http.MethodHead {
			return mediaTestResponse(r, 200, http.Header{"Content-Type": {"image/jpeg"}, "Content-Length": {"1234"}}, nil), nil
		}
		if r.Header.Get("Range") != "bytes=2-5" {
			t.Fatal("Range not forwarded")
		}
		return mediaTestResponse(r, 206, http.Header{"Content-Type": {"image/jpeg"}, "Content-Range": {"bytes 2-5/8"}, "Content-Length": {"4"}}, []byte("2345")), nil
	}))
	redirect := redirectResp("https://fbcdn.net/range-test", 302)
	r := testMediaRequest(http.MethodGet, "/offload/example?preview=1")
	r.Header.Set("Range", "bytes=2-5")
	r.Header.Set("Cookie", "secret=keep-origin-only")
	r.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	if !proxyOffloadMedia(response, r, redirect) || response.Code != 206 || response.Body.String() != "2345" || response.Header().Get("Content-Range") != "bytes 2-5/8" {
		t.Fatal("video range metadata or bytes changed")
	}
	head := testMediaRequest(http.MethodHead, "/offload/example?preview=1")
	head.Header.Set("Range", "bytes=2-5")
	response = httptest.NewRecorder()
	if !proxyOffloadMedia(response, head, redirect) || response.Body.Len() != 0 || response.Header().Get("Content-Length") != "1234" {
		t.Fatal("HEAD response metadata not preserved")
	}
	if proxyOffloadMedia(httptest.NewRecorder(), testMediaRequest(http.MethodGet, "/offload/example"), redirect) {
		t.Fatal("non-preview media streamed through the origin")
	}
}

func TestMediaStreamExtendsServerWriteDeadline(t *testing.T) {
	payload := []byte("complete delayed image")
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			time.Sleep(100 * time.Millisecond)
			_, _ = writer.Write(payload)
		}()
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"image/jpeg"}, "Content-Length": {strconv.Itoa(len(payload))}},
			Body:          reader,
			ContentLength: int64(len(payload)),
			Request:       r,
		}, nil
	}))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !proxyOffloadMedia(w, r, redirectResp("https://cdninstagram.com/slow-video", 302)) {
			http.Error(w, "media was not handled", http.StatusBadGateway)
		}
	}))
	server.Config.WriteTimeout = 25 * time.Millisecond
	server.Start()
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/offload/example?preview=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("User-Agent", "Mozilla/5.0")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("media stream hit the general server write deadline: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("delayed media stream was truncated: status=%d body=%q error=%v", response.StatusCode, body, err)
	}
}

func TestPreviewImageLimitsAndNegotiation(t *testing.T) {
	for _, tc := range []struct{ accept, format string }{
		{"image/avif,image/webp", "avif"},
		{"image/avif;q=0,image/webp", "webp"},
		{"image/avif;q=0.4,image/webp;q=0.8", "webp"},
		{"image/webp;q=0", ""},
		{"image/jpeg,*/*", ""},
	} {
		if got := preferredPreviewFormat(tc.accept); got != tc.format {
			t.Errorf("%s negotiated %s, want %s", tc.accept, got, tc.format)
		}
	}
	body := testPreviewPNG(t, 1, 1)
	// Keep the PNG chunk CRC valid so DecodeConfig sees the advertised canvas.
	binary.BigEndian.PutUint32(body[16:20], maxPreviewDimension+1)
	binary.BigEndian.PutUint32(body[29:33], crc32.ChecksumIEEE(body[12:29]))
	if _, _, err := transformPreview(context.Background(), body, 96, ""); err == nil {
		t.Fatal("oversized image accepted")
	}
	if out, typ, err := transformPreview(context.Background(), testPreviewPNG(t, 32, 16), 96, ""); err != nil || typ != "image/png" {
		t.Fatalf("small preview failed: %s %v", typ, err)
	} else if cfg, _, err := image.DecodeConfig(bytes.NewReader(out)); err != nil || cfg.Width != 32 {
		t.Fatal("small images should not be upscaled")
	}
}

func TestMediaUnavailableEncoderPreservesOriginal(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	body := testPreviewPNG(t, 200, 100)
	stubMediaTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mediaTestResponse(r, 200, http.Header{"Content-Type": {"image/png"}}, body), nil
	}))
	r := testMediaRequest(http.MethodGet, "/offload/example?preview=avatar")
	r.Header.Set("Accept", "image/avif")
	response := httptest.NewRecorder()
	if !proxyOffloadMedia(response, r, redirectResp("https://fbcdn.net/no-encoder-test", 302)) || !bytes.Equal(response.Body.Bytes(), body) || response.Header().Get("Content-Type") != "image/png" {
		t.Fatal("encoder failure silently changed the representation")
	}
}

func TestPreviewCacheBounded(t *testing.T) {
	cache := mediaPreviewCache{entries: make(map[string]previewMedia)}
	for index := 0; index < 140; index++ {
		cache.put(strings.Repeat("x", index+1), previewMedia{body: []byte("image"), expires: time.Now().Add(time.Hour)})
	}
	if len(cache.entries) != 128 || cache.bytes != 128*5 {
		t.Fatalf("entry limit not bounded: entries=%d bytes=%d", len(cache.entries), cache.bytes)
	}
	cache.put("large", previewMedia{body: make([]byte, mediaCacheBytes), expires: time.Now().Add(time.Hour)})
	if len(cache.entries) != 1 || cache.bytes != mediaCacheBytes {
		t.Fatal("byte limit not bounded")
	}
	cache.put("expired", previewMedia{body: []byte("x"), expires: time.Now().Add(-time.Second)})
	if _, ok := cache.get("expired"); ok {
		t.Fatal("expired preview was returned")
	}
}

func TestPreviewCacheClearDropsTransformedMedia(t *testing.T) {
	cache := mediaPreviewCache{entries: make(map[string]previewMedia)}
	cache.put("first", previewMedia{body: []byte("old image"), expires: time.Now().Add(time.Hour)})
	cache.put("second", previewMedia{body: []byte("old avatar"), expires: time.Now().Add(time.Hour)})
	cache.clear()
	if _, ok := cache.get("first"); ok {
		t.Fatal("purged preview remains cached")
	}
	if _, ok := cache.get("second"); ok {
		t.Fatal("purged avatar remains cached")
	}
	if cache.bytes != 0 || len(cache.entries) != 0 {
		t.Fatal("purged preview storage was not released")
	}
	cache.put("first", previewMedia{body: []byte("fresh image"), expires: time.Now().Add(time.Hour)})
	if fresh, ok := cache.get("first"); !ok || string(fresh.body) != "fresh image" {
		t.Fatal("preview cache cannot be reused after purge")
	}
}

func TestMediaBrowserTTLDoesNotExceedSignedExpiry(t *testing.T) {
	request := testMediaRequest(http.MethodGet, "/offload/example?exp="+strconv.FormatInt(time.Now().Add(30*time.Second).Unix(), 10))
	headers := make(http.Header)
	setMediaHeaders(headers, http.Header{"Content-Type": {"image/jpeg"}}, false, request)
	ttl, err := strconv.Atoi(strings.TrimPrefix(headers.Get("Cache-Control"), "public, max-age="))
	if err != nil || ttl < 1 || ttl > 30 || headers.Get("Cloudflare-CDN-Cache-Control") != "no-store" {
		t.Fatalf("unsafe signed media cache policy: %v", headers)
	}
	request.URL.RawQuery = "exp=1"
	setMediaHeaders(headers, make(http.Header), false, request)
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatal("expired capability was made browser-cacheable")
	}
}

func TestAVIFPreviewKeepsTransparency(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	img.Set(0, 0, color.NRGBA{255, 0, 0, 128})
	var original bytes.Buffer
	if err := png.Encode(&original, img); err != nil {
		t.Fatal(err)
	}
	body, contentType, err := transformPreview(context.Background(), original.Bytes(), 96, "avif")
	if err != nil || contentType != "image/png" {
		t.Fatalf("transparent PNG fallback missing: %s %v", contentType, err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, alpha := decoded.At(0, 0).RGBA()
	if alpha != 128*257 {
		t.Fatal("preview transparency changed")
	}
}

func TestModernPreviewEncoders(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg integration requires the deployment runtime")
	}
	body := testPreviewPNG(t, 200, 100)
	for _, format := range []string{"webp", "avif"} {
		out, typ, err := transformPreview(context.Background(), body, 96, format)
		if err != nil {
			t.Fatalf("%s encoder failed: %v", format, err)
		}
		if typ != "image/"+format || len(out) < 12 {
			t.Fatal("incorrect encoded image metadata")
		}
		if format == "webp" && (string(out[:4]) != "RIFF" || string(out[8:12]) != "WEBP") {
			t.Fatal("output is not WebP")
		}
		if format == "avif" && !bytes.Contains(out[:min(len(out), 64)], []byte("avif")) {
			t.Fatal("output is not AVIF")
		}
	}
}

func TestMediaDialerRejectsNonPublicAddresses(t *testing.T) {
	for _, tt := range []struct {
		address string
		public  bool
	}{
		{"127.0.0.1:443", false}, {"[::1]:443", false}, {"10.0.0.1:443", false}, {"172.17.0.1:443", false},
		{"192.168.1.1:443", false}, {"100.64.1.1:443", false}, {"169.254.169.254:443", false}, {"[fd00::1]:443", false},
		{"0.0.0.0:443", false}, {"[::ffff:127.0.0.1]:443", false},
		{"157.240.1.1:443", true}, {"[2a03:2880::1]:443", true},
	} {
		if err := mediaDialer.Control("tcp", tt.address, nil); (err == nil) != tt.public {
			t.Errorf("Control(%s) = %v, public %v", tt.address, err, tt.public)
		}
	}
}
