package main

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// trustedMediaURL accepts only HTTPS Instagram CDN URLs as redirect targets.
func trustedMediaURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "cdninstagram.com" || strings.HasSuffix(host, ".cdninstagram.com") ||
		host == "fbcdn.net" || strings.HasSuffix(host, ".fbcdn.net")
}

func (a *App) offloadResp(req *http.Request, target string, avatar bool) resp {
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	if avatar {
		return avatarResp(req.Context(), target)
	}
	return redirectResp(target, 302)
}

var avatarTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif"}

// Instagram serves profile pictures with Cross-Origin-Resource-Policy:
// same-origin, so pages here can only show them from this origin. Bots fetch
// server-side, so when the copy fails they still get the redirect.
func avatarResp(ctx context.Context, target string) resp {
	u, err := url.Parse(target)
	if err != nil || !trustedMediaURL(u) {
		return redirectResp(target, 302)
	}
	contentType, body, ferr := fetchAvatar(ctx, target)
	if ferr != nil {
		// Briefly, so a failing CDN doesn't send every repeat hit to the origin.
		r := redirectResp(target, 302)
		r.originStatus, r.errorType = ferr.Status, ferr.Code
		r.headers["Cloudflare-CDN-Cache-Control"] = "max-age=60"
		return r
	}
	return resp{status: http.StatusOK, headers: map[string]string{"Content-Type": contentType}, body: body, mediaURL: target}
}

// The CDN is public, so this goes direct, never through the paid proxy pool.
func fetchAvatar(ctx context.Context, rawURL string) (contentType string, body []byte, ferr *AppError) {
	ctx, cancel := context.WithTimeout(ctx, avatarTimeout)
	defer cancel()
	started, status := time.Now(), 0
	defer func() {
		logOutbound(ctx, "avatar", "direct", http.MethodGet, rawURL, started, status, len(body), ferr, false)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, causedErr(500, "", "internal request error", err)
	}
	client := *http.DefaultClient
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !trustedMediaURL(r.URL) {
			return errors.New("avatar redirected off the CDN")
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, contextAppError(ctx)
		}
		return "", nil, requestError(err, "avatar request failed")
	}
	defer res.Body.Close()
	status = res.StatusCode
	if err := statusOnly(status, nil); err != nil {
		return "", nil, err
	}
	contentType, _, _ = mime.ParseMediaType(res.Header.Get("Content-Type"))
	if !slices.Contains(avatarTypes, contentType) {
		return "", nil, igErr(http.StatusBadGateway, errorCodeUpstream, "avatar is not an image")
	}
	body, err = io.ReadAll(io.LimitReader(countingReader{res.Body, outboundCounter("avatar")}, maxAvatarBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, contextAppError(ctx)
		}
		return "", nil, requestError(err, "avatar read failed")
	}
	if len(body) > maxAvatarBytes {
		return "", nil, igErr(http.StatusBadGateway, errorCodeUpstream, "avatar too large")
	}
	return contentType, body, nil
}
