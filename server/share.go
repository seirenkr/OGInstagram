package main

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type shareTarget string

const shareCacheTTL = 24 * time.Hour

func instagramShareURL(kind, token string, mediaIndex int, specified bool) string {
	target := instagramOrigin + "/share/"
	if kind == "p" || kind == "reel" {
		target += kind + "/"
	}
	target += url.PathEscape(token) + "/"
	if specified {
		target += "?img_index=" + strconv.Itoa(mediaIndex+1)
	}
	return target
}

func isInstagramShareHost(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "www.instagram.com", "instagram.com", "m.instagram.com":
		return true
	default:
		return false
	}
}

func isShareRedirectHop(prev, next *url.URL) bool {
	return next != nil && next.Scheme == "https" && strings.EqualFold(next.Hostname(), "www.instagram.com") &&
		next.Port() == "" && next.User == nil && strings.HasPrefix(next.Path, "/share/") && next.String() != prev.String()
}

// Non-browser UAs get a 302 Location (and 200 HTML for missing tokens), so HEAD resolves with zero body.
func shareSpec(token string) fetchSpec {
	return fetchSpec{
		operation: "share",
		method:    http.MethodHead,
		url:       instagramOrigin + "/share/" + url.PathEscape(token) + "/",
		subject:   "Instagram",
		redirect:  true,
		headers:   map[string]string{"User-Agent": embedUA},
		interpret: func(status int, raw []byte) *AppError {
			switch {
			case status >= 300 && status < 400:
				_, err := parseShareTarget(string(raw))
				return err
			case status == http.StatusOK || status == http.StatusNotFound:
				return igErr(http.StatusNotFound, errorCodeNotFound, "share link not found")
			case status == http.StatusTooManyRequests:
				return igErr(status, errorCodeRateLimited, http.StatusText(status))
			default:
				return igErr(status, errorCodeUpstream, http.StatusText(status))
			}
		},
	}
}

func parseShareTarget(rawURL string) (shareTarget, *AppError) {
	u, err := url.Parse(rawURL)
	if err != nil || !isInstagramShareHost(u) {
		return "", igErr(http.StatusBadGateway, errorCodeUpstream, "Instagram returned an unexpected share redirect")
	}
	if isLoginWallPath(u.Path) {
		return "", igErr(http.StatusBadGateway, errorCodeLoginRequired, "Instagram redirected to login or verification")
	}
	segments := splitPath(u.Path)
	if username, id, ok := parseStorySegments(segments); ok {
		return shareTarget("/stories/" + username + "/" + id), nil
	}
	if post := parseEmbedSegments(segments); post != nil {
		if !validPostShortcode(post.Shortcode) {
			return "", igErr(http.StatusNotFound, errorCodeNotFound, "invalid shortcode")
		}
		target := "/" + post.PostType + "/" + post.Shortcode
		if index, specified := mediaSelection(u.Query(), post.PathIndex); specified {
			target += "?img_index=" + strconv.Itoa(index+1)
		}
		return shareTarget(target), nil
	}
	if len(segments) == 1 && isProfileUsername(segments[0]) {
		return shareTarget("/" + segments[0]), nil
	}
	return "", igErr(http.StatusNotFound, errorCodeNotFound, "share link target is not supported")
}

func (a *App) getShareTarget(ctx context.Context, token string, meta *fetchMeta) (shareTarget, *AppError) {
	if !validShortcode(token) {
		return "", igErr(http.StatusNotFound, errorCodeNotFound, "invalid share token")
	}
	return a.shares.get(ctx, token, meta, func(fetchCtx context.Context) (shareTarget, time.Duration, bool, *AppError) {
		_, location, err := a.fetchViaProxy(fetchCtx, shareSpec(token))
		if err != nil {
			return "", 0, false, err
		}
		target, err := parseShareTarget(location)
		return target, shareCacheTTL, true, err
	})
}

func (a *App) shareErrorResp(req *http.Request, origin string, err *AppError, meta *fetchMeta, direct bool) resp {
	if direct {
		return tagFetch(offloadErrorResp(err, origin), meta)
	}
	if wantsActivity(req) {
		return tagFetch(activityErrorResp(err), meta)
	}
	title, desc := errorCard("link", err.Code)
	return a.errorCardResp(a.publicBaseURL(req), origin, title, desc, err.Code, err, meta)
}

func (a *App) handleShare(req *http.Request, share ShareRoute, index int, specified, direct bool) resp {
	origin := instagramShareURL(share.Kind, share.Token, index, specified)
	shareMeta := &fetchMeta{}
	target, err := a.getShareTarget(req.Context(), share.Token, shareMeta)
	if err != nil {
		return a.shareErrorResp(req, origin, err, shareMeta, direct)
	}
	u, parseErr := url.Parse(string(target))
	if parseErr != nil {
		return a.shareErrorResp(req, origin, igErr(http.StatusNotFound, errorCodeNotFound, "share link target is not supported"), shareMeta, direct)
	}
	q := u.Query()
	if specified {
		q.Set("img_index", strconv.Itoa(index+1))
	}
	for _, key := range []string{"thumbnail", "e"} {
		if req.URL.Query().Has(key) {
			q.Set(key, req.URL.Query().Get(key))
		}
	}
	u.RawQuery = q.Encode()

	targetReq := req.Clone(req.Context())
	targetReq.URL = u
	route, ok := resolveGatewayRoute(targetReq)
	if !ok {
		return a.shareErrorResp(req, origin, igErr(http.StatusNotFound, errorCodeNotFound, "share link target is not supported"), shareMeta, direct)
	}
	r := route.handle(a, targetReq)
	r.category = route.category
	if shareMeta.fetched {
		r.cacheHit = false
	}
	return r
}
