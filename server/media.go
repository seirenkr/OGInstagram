package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxMediaRedirects    = 3
	maxPreviewBytes      = 8 << 20
	maxPreviewPixels     = 12_000_000
	maxPreviewDimension  = 16384
	mediaCacheBytes      = 8 << 20
	mediaTransferTimeout = 60 * time.Second
	mediaWriteTimeout    = mediaTransferTimeout + 5*time.Second
)

var mediaTransformSlot = make(chan struct{}, 1)

var mediaHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:            mediaDialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           24,
		MaxIdleConnsPerHost:    4,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       mediaTransferTimeout,
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// The URL allowlist is checked before every request. Control sees the actual
// connect address, so a CDN hostname can never reach the VM or its private network.
var mediaDialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		if a := ap.Addr().Unmap(); !a.IsGlobalUnicast() || a.IsPrivate() || cgnat.Contains(a) {
			return errors.New("non-public media address")
		}
		return nil
	}}

func trustedMediaURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "cdninstagram.com" || strings.HasSuffix(host, ".cdninstagram.com") ||
		host == "fbcdn.net" || strings.HasSuffix(host, ".fbcdn.net")
}

// Returning false leaves the existing signed CDN redirect intact. Fetch errors
// never turn an otherwise usable offload URL into a failing response.
func proxyOffloadMedia(w http.ResponseWriter, r *http.Request, redirect resp) bool {
	if redirect.status != http.StatusFound || !strings.HasPrefix(r.URL.Path, "/offload/") ||
		r.Header.Get("Sec-Fetch-Site") != "same-origin" || gatewayIsBot(r.UserAgent()) {
		return false
	}
	target, err := url.Parse(redirect.headers["Location"])
	if err != nil || !trustedMediaURL(target) {
		return false
	}
	// Only the home page preview images are proxied; everything else keeps the
	// CDN redirect so videos and originals never stream through the VM.
	width := 0
	switch r.URL.Query().Get("preview") {
	case "avatar":
		width = 96
	case "1":
		width = 720
	default:
		return false
	}
	// The general server write deadline is ten seconds. Media transfers have
	// their own longer deadline, and a separate context bounds upstream reads.
	// Test recorders may not implement deadline control; real net/http writers do.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(mediaWriteTimeout))
	format := preferredPreviewFormat(r.Header.Get("Accept"))
	cacheKey := target.String() + "|" + strconv.Itoa(width) + "|" + format
	transform := r.Header.Get("Range") == ""
	if transform {
		if cached, ok := previewCache.get(cacheKey); ok {
			writeMediaPreview(w, r, cached)
			return true
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), mediaTransferTimeout)
	defer cancel()
	// At most one decoded image and one encoder process can exist on a 1 GiB VM.
	// Busy requests still stream the original CDN representation.
	if transform {
		select {
		case mediaTransformSlot <- struct{}{}:
			defer func() { <-mediaTransformSlot }()
		default:
			transform = false
		}
	}
	method := r.Method
	if transform {
		method = http.MethodGet
	}
	upstream, err := loadMedia(ctx, target, method, r.Header)
	if err != nil {
		return false
	}
	defer upstream.Body.Close()
	if upstream.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		setMediaHeaders(w.Header(), upstream.Header, r)
		w.Header().Del("Content-Length")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return true
	}
	if !safeMediaContentType(upstream.Header.Get("Content-Type")) {
		return false
	}
	if transform && upstream.StatusCode == http.StatusOK && (upstream.ContentLength < 0 || upstream.ContentLength <= maxPreviewBytes) {
		body, readErr := io.ReadAll(io.LimitReader(upstream.Body, maxPreviewBytes+1))
		if readErr != nil {
			return false
		}
		if len(body) <= maxPreviewBytes {
			encoded, contentType, err := transformPreview(ctx, body, width, format)
			if err == nil {
				entry := previewMedia{body: encoded, contentType: contentType, expires: time.Now().Add(time.Hour)}
				previewCache.put(cacheKey, entry)
				writeMediaPreview(w, r, entry)
				return true
			}
		}
		// Unsupported formats, oversized images, or unavailable encoders retain
		// their actual representation and Content-Type instead of relabeling data.
		setMediaHeaders(w.Header(), upstream.Header, r)
		if len(body) <= maxPreviewBytes {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		} else {
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(upstream.StatusCode)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
			if len(body) > maxPreviewBytes {
				_, _ = io.Copy(w, upstream.Body)
			}
		}
		return true
	}
	setMediaHeaders(w.Header(), upstream.Header, r)
	w.WriteHeader(upstream.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, upstream.Body)
	}
	return true
}

func loadMedia(ctx context.Context, target *url.URL, method string, headers http.Header) (*http.Response, error) {
	current := target
	headerDeadline := time.Now().Add(10 * time.Second)
	for hop := 0; hop <= maxMediaRedirects; hop++ {
		if !trustedMediaURL(current) {
			return nil, errors.New("untrusted media destination")
		}
		remaining := time.Until(headerDeadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		requestCtx, cancel := context.WithCancel(ctx)
		request, err := http.NewRequestWithContext(requestCtx, method, current.String(), nil)
		if err != nil {
			cancel()
			return nil, err
		}
		request.Header.Set("Accept-Encoding", "identity")
		request.Header.Set("User-Agent", "OGInstagram/1.0")
		if value := headers.Get("Range"); value != "" {
			request.Header.Set("Range", value)
		}
		if value := headers.Get("If-Range"); value != "" {
			request.Header.Set("If-Range", value)
		}
		// Stop the header timer once headers arrive so it does not interrupt
		// streaming a video. The parent context still bounds the entire transfer.
		timer := time.AfterFunc(remaining, cancel)
		response, err := mediaHTTPClient.Do(request)
		timer.Stop()
		if err != nil {
			cancel()
			return nil, err
		}
		if requestCtx.Err() != nil {
			response.Body.Close()
			cancel()
			return nil, requestCtx.Err()
		}
		response.Body = &mediaBody{ReadCloser: response.Body, cancel: cancel}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			location := response.Header.Get("Location")
			response.Body.Close()
			if location == "" || hop == maxMediaRedirects {
				return nil, errors.New("media redirect limit")
			}
			next, err := current.Parse(location)
			if err != nil || !trustedMediaURL(next) {
				return nil, errors.New("untrusted media redirect")
			}
			current = next
			continue
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent && response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			response.Body.Close()
			return nil, errors.New("media upstream unavailable")
		}
		return response, nil
	}
	return nil, errors.New("media redirect limit")
}

type mediaBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *mediaBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func safeMediaContentType(value string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
	return strings.HasPrefix(mediaType, "image/") && mediaType != "image/svg+xml"
}

func setMediaHeaders(target, source http.Header, r *http.Request) {
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Content-Encoding", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
	if target.Get("Content-Type") == "" {
		target.Set("Content-Type", "application/octet-stream")
	}
	ttl := int64(3600)
	if raw := r.URL.Query().Get("exp"); raw != "" {
		expires, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			ttl = 0
		} else {
			ttl = min(ttl, max(int64(0), expires-time.Now().Unix()))
		}
	}
	if ttl == 0 {
		target.Set("Cache-Control", "no-store")
	} else {
		target.Set("Cache-Control", "public, max-age="+strconv.FormatInt(ttl, 10))
	}
	// Verification must run for every signed capability, including CDN hits.
	target.Set("Cloudflare-CDN-Cache-Control", "no-store")
	target.Set("Cross-Origin-Resource-Policy", "cross-origin")
	target.Set("Vary", "Accept")
}

func preferredPreviewFormat(accept string) string {
	var avif, webp float64
	for _, item := range strings.Split(strings.ToLower(accept), ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && key == "q" {
				var err error
				quality, err = strconv.ParseFloat(value, 64)
				if err != nil || quality < 0 || quality > 1 {
					quality = 0
				}
			}
		}
		switch parts[0] {
		case "image/avif":
			avif = quality
		case "image/webp":
			webp = quality
		}
	}
	if avif > 0 && avif >= webp {
		return "avif"
	}
	if webp > 0 {
		return "webp"
	}
	return ""
}

func transformPreview(ctx context.Context, body []byte, width int, format string) ([]byte, string, error) {
	config, sourceFormat, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	if config.Width < 1 || config.Height < 1 || config.Width > maxPreviewDimension || config.Height > maxPreviewDimension ||
		int64(config.Width)*int64(config.Height) > maxPreviewPixels {
		return nil, "", errors.New("preview dimensions exceeded")
	}
	if sourceFormat != "jpeg" && sourceFormat != "png" && sourceFormat != "webp" {
		return nil, "", errors.New("unsupported preview format")
	}
	original, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	resized := original
	if config.Width > width {
		height := max(1, int(int64(config.Height)*int64(width)/int64(config.Width)))
		output := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.BiLinear.Scale(output, output.Bounds(), original, original.Bounds(), draw.Src, nil)
		resized = output
	}
	var encoded limitedMediaBuffer
	// The single-stream AVIF encoder cannot preserve an alpha plane. Keep a
	// resized PNG for transparent images rather than flattening them silently.
	if format == "avif" {
		if opacity, ok := resized.(interface{ Opaque() bool }); ok && !opacity.Opaque() {
			err = png.Encode(&encoded, resized)
			return encoded.Bytes(), "image/png", err
		}
	}
	if format == "avif" || format == "webp" {
		// ffmpeg reads this intermediate in-process; compressing it only burns CPU.
		if err := (&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&encoded, resized); err != nil {
			return nil, "", err
		}
		out, err := encodeModernPreview(ctx, encoded.Bytes(), format)
		return out, "image/" + format, err
	}
	if sourceFormat == "jpeg" {
		err = jpeg.Encode(&encoded, resized, &jpeg.Options{Quality: 85})
		return encoded.Bytes(), "image/jpeg", err
	}
	err = png.Encode(&encoded, resized)
	return encoded.Bytes(), "image/png", err
}

// FFmpeg is included in the deployment image with libwebp and libaom-av1. An
// image is decoded and resized in Go first; the subprocess sees only bounded
// PNG bytes, never a URL or an arbitrary media container.
func encodeModernPreview(parent context.Context, pngBody []byte, format string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-filter_threads", "1", "-filter_complex_threads", "1", "-threads", "1", "-f", "image2pipe", "-c:v", "png", "-i", "pipe:0", "-frames:v", "1", "-an", "-threads", "1"}
	name := "" // AVIF output file; WebP streams to stdout
	if format == "avif" {
		// The AVIF muxer requires a seekable output, unlike WebP's stream muxer.
		f, err := os.CreateTemp("", "oginstagram-preview-*.avif")
		if err != nil {
			return nil, err
		}
		name = f.Name()
		f.Close()
		defer os.Remove(name)
		args = append(args, "-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "35", "-still-picture", "1", "-pix_fmt", "yuv420p", "-f", "avif", "-y", name)
	} else {
		args = append(args, "-c:v", "libwebp", "-quality", "80", "-f", "webp", "pipe:1")
	}
	var output limitedMediaBuffer
	command := exec.CommandContext(ctx, "ffmpeg", args...)
	command.Stdin = bytes.NewReader(pngBody)
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, err
	}
	if name == "" {
		if output.Len() == 0 {
			return nil, errors.New("empty WebP encoding")
		}
		return output.Bytes(), nil
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	encoded, err := io.ReadAll(io.LimitReader(f, maxPreviewBytes+1))
	if err != nil || len(encoded) == 0 || len(encoded) > maxPreviewBytes {
		return nil, errors.New("invalid AVIF encoding size")
	}
	return encoded, nil
}

type limitedMediaBuffer struct{ bytes.Buffer }

func (b *limitedMediaBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxPreviewBytes {
		return 0, errors.New("preview output too large")
	}
	return b.Buffer.Write(p)
}

type previewMedia struct {
	body        []byte
	contentType string
	expires     time.Time
}

type mediaPreviewCache struct {
	sync.Mutex
	entries map[string]previewMedia
	bytes   int
}

var previewCache = mediaPreviewCache{entries: make(map[string]previewMedia)}

func (c *mediaPreviewCache) clear() {
	c.Lock()
	defer c.Unlock()
	clear(c.entries)
	c.bytes = 0
}

func (c *mediaPreviewCache) get(key string) (previewMedia, bool) {
	c.Lock()
	defer c.Unlock()
	entry, ok := c.entries[key]
	if ok && !time.Now().Before(entry.expires) {
		c.bytes -= len(entry.body)
		delete(c.entries, key)
		return previewMedia{}, false
	}
	return entry, ok
}

func (c *mediaPreviewCache) put(key string, entry previewMedia) {
	if len(entry.body) > mediaCacheBytes {
		return
	}
	c.Lock()
	defer c.Unlock()
	if previous, ok := c.entries[key]; ok {
		c.bytes -= len(previous.body)
		delete(c.entries, key)
	}
	for c.bytes+len(entry.body) > mediaCacheBytes || len(c.entries) >= 128 {
		oldestKey := ""
		var oldest time.Time
		for key, candidate := range c.entries {
			if oldestKey == "" || candidate.expires.Before(oldest) {
				oldestKey, oldest = key, candidate.expires
			}
		}
		c.bytes -= len(c.entries[oldestKey].body)
		delete(c.entries, oldestKey)
	}
	c.entries[key] = entry
	c.bytes += len(entry.body)
}

func writeMediaPreview(w http.ResponseWriter, r *http.Request, entry previewMedia) {
	setMediaHeaders(w.Header(), http.Header{"Content-Type": {entry.contentType}}, r)
	checksum := sha256.Sum256(entry.body)
	w.Header().Set("ETag", `"`+hex.EncodeToString(checksum[:])+`"`)
	http.ServeContent(w, r, "preview", time.Time{}, bytes.NewReader(entry.body))
}
