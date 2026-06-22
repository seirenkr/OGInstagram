package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	cfg := configFromEnv()
	assets, err := loadAssets(cfg.AssetsDir)
	if err != nil {
		slog.Error("assets_load_failed", "dir", cfg.AssetsDir, "err", err.Error())
		os.Exit(1)
	}
	app := newApp(cfg, newSessionPool(cfg), assets)

	slog.Info("container_start", "service", serviceName, "version", cfg.Version,
		"proxies", len(app.pool.sessions), "port", cfg.Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handle)

	if err := http.ListenAndServe("0.0.0.0:"+strconv.Itoa(cfg.Port), mux); err != nil {
		slog.Error("server_exit", "err", err.Error())
		os.Exit(1)
	}
}

type resp struct {
	status  int
	headers map[string]string
	body    []byte
}

func (a *App) write(w http.ResponseWriter, r resp) {
	for k, v := range r.headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(r.status)
	if r.body != nil {
		_, _ = w.Write(r.body)
	}
}

func htmlResp(status int, body string) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "text/html; charset=utf-8"}, body: []byte(body)}
}
func jsonResp(status int, body []byte) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "application/json"}, body: body}
}
func textResp(status int, text string) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}, body: []byte(text)}
}
func redirectResp(location string, status int) resp {
	return resp{status: status, headers: map[string]string{"Location": location, "Content-Type": "text/plain; charset=utf-8"}}
}

func cacheable(r resp, seconds int) resp {
	r.headers["Cache-Control"] = "public, s-maxage=" + strconv.Itoa(seconds)
	return r
}
func cacheableHome(r resp) resp {
	r.headers["Cache-Control"] = "public, max-age=" + strconv.Itoa(homeBrowserCacheSecs) + ", s-maxage=" + strconv.Itoa(homeEdgeCacheSeconds)
	return r
}
func tagFetch(r resp, meta *fetchMeta) resp {
	if meta.fetched {
		r.headers["x-og-cache"] = "miss"
	} else {
		r.headers["x-og-cache"] = "hit"
	}
	return r
}

func (a *App) handle(w http.ResponseWriter, req *http.Request) {
	r := a.route(req)
	a.write(w, r)
}

func (a *App) route(req *http.Request) resp {
	path := req.URL.Path

	if path == "/" {
		return cacheableHome(htmlResp(200, a.buildHomeHTML(req.Host, req.Header.Get("Accept-Language"), nil)))
	}
	if loc, ok := homePathLocale(path); ok {
		l := loc
		return cacheableHome(htmlResp(200, a.buildHomeHTML(req.Host, req.Header.Get("Accept-Language"), &l)))
	}
	if path == "/_container/health" {
		return jsonResp(200, mustJSON(map[string]any{"ok": true, "service": serviceName + "-container"}))
	}
	if path == "/_status" && a.cfg.LocalPreview {
		return cacheable(jsonResp(200, localPreviewStatus(time.Now())), 60)
	}
	if f, ok := a.assets.static(path); ok {
		return cacheable(resp{status: 200, headers: map[string]string{"Content-Type": f.contentType}, body: f.body}, iconCacheSeconds)
	}
	if path == "/oembed" {
		return a.handleOEmbed(req)
	}
	if path == "/owoembed" {
		return cacheable(jsonResp(200, a.buildOwOEmbed(a.publicBaseURL(req), parseRequestQuery(req.URL.Query()))), edgeCacheSeconds)
	}

	segments := splitPath(path)
	if (len(segments) == 2 || len(segments) == 3) && segments[0] == "offload" {
		return a.handleOffload(req, segments)
	}
	if len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "statuses" {
		return a.handleActivity(req, segments[3], false)
	}
	if len(segments) == 2 && segments[0] == "statuses" {
		return a.handleActivity(req, segments[1], false)
	}
	if len(segments) == 4 && segments[0] == "users" && segments[2] == "statuses" {
		return a.handleActivity(req, segments[3], false)
	}
	if len(segments) == 2 && segments[0] == "users" {
		return cacheable(jsonResp(200, a.buildActivityAccount(segments[1])), edgeCacheSeconds)
	}
	if route := parseEmbedSegments(segments); route != nil {
		pathIndex := -1
		if route.HasIndex {
			pathIndex = route.PathIndex
		}
		return a.handleEmbed(req, route.PostType, route.Shortcode, pathIndex)
	}
	return resp{status: 404, headers: map[string]string{}}
}

func (a *App) handleEmbed(req *http.Request, postType, shortcode string, pathIndex int) resp {
	q := parseRequestQuery(req.URL.Query())
	mediaIndex := mediaIndexFromQuery(q, pathIndex)
	specified := pathIndex >= 0 || querySpecified(q)
	origin := instagramURLForSelection(postType, shortcode, mediaIndex, specified)

	if !botRE.MatchString(req.Header.Get("User-Agent")) {
		return redirectResp(origin, 307)
	}

	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	post, err := a.getPost(shortcode, meta)
	if err != nil {
		reason := err.Reason
		title, desc := errorCard(reason)

		if reason == reasonMediaNotFound || reason == reasonNotFound {
			if r2, t2, d2, ok := a.oembedRefine(shortcode); ok {
				reason, title, desc = r2, t2, d2
			}
		}
		embed := a.buildStatusEmbedHTML(baseURL, origin, title, desc)
		r := htmlResp(200, embed)
		r.headers["x-og-status"] = strconv.Itoa(err.Status)
		if reason != "" {
			r.headers["x-og-reason"] = reason
		}
		if !isTransientStatus(err.Status) {
			r = cacheable(r, errorEmbedCacheSecond)
		}
		return tagFetch(r, meta)
	}
	html := a.buildEmbedHTML(baseURL, req.Header.Get("User-Agent"), post, postType, mediaIndex, specified, q.Gallery)
	return tagFetch(cacheable(htmlResp(200, html), edgeCacheSeconds), meta)
}

func (a *App) handleOffload(req *http.Request, segments []string) resp {
	shortcode := segments[1]
	index := 0
	if len(segments) == 3 {
		if n, err := strconv.Atoi(segments[2]); err == nil && n > 0 {
			index = n - 1
		}
	}
	thumbnail := req.URL.Query().Has("thumbnail")
	meta := &fetchMeta{}
	post, err := a.getPost(shortcode, meta)
	if err != nil {
		if isTransientStatus(err.Status) {
			return tagFetch(redirectResp(instagramOrigin+"/p/"+pathEscape(shortcode)+"/", 302), meta)
		}
		return resp{status: err.Status, headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}, body: []byte(err.Message)}
	}
	att := post.Attachments[mediaIndexFor(post, index)]
	target := att.URL
	if thumbnail {
		if att.Thumbnail != "" {
			target = att.Thumbnail
		}
	}
	return tagFetch(cacheable(redirectResp(target, 302), edgeCacheSeconds), meta)
}

func (a *App) handleActivity(req *http.Request, statusID string, gallery bool) resp {
	q := parseRequestQuery(req.URL.Query())
	route := parseActivityCode(statusID)
	gallery = gallery || q.Gallery || route.Gallery
	if q.Shortcode != "" {
		route.Shortcode = q.Shortcode
	}
	if q.PostType != "" {
		route.PostType = normalizePostType(q.PostType)
	}
	if querySpecified(q) {
		route.MediaIndex = mediaIndexFromQuery(q, -1)
		route.MediaIndexSpecified = true
	}
	post, err := a.getPost(route.Shortcode, nil)
	if err != nil {
		if isTransientStatus(err.Status) {
			return jsonResp(err.Status, a.buildRateLimitActivityStatus(route.Shortcode))
		}
		return textResp(err.Status, err.Message)
	}
	body := a.buildActivityStatus(a.publicBaseURL(req), statusID, post, route.PostType, route.MediaIndex, route.MediaIndexSpecified, gallery)
	return cacheable(jsonResp(200, body), edgeCacheSeconds)
}

func (a *App) handleOEmbed(req *http.Request) resp {
	q := parseRequestQuery(req.URL.Query())
	fillQueryFromURL(&q, req.URL.Query().Get("url"))
	if q.Shortcode == "" {
		return textResp(400, "shortcode required")
	}
	postType := normalizePostType(q.PostType)
	mediaIndex := mediaIndexFromQuery(q, -1)
	specified := querySpecified(q)
	baseURL := a.publicBaseURL(req)
	post, err := a.getPost(q.Shortcode, nil)
	if err != nil {
		if isTransientStatus(err.Status) {
			return jsonResp(err.Status, a.buildRateLimitOEmbed(baseURL, postType, q.Shortcode, mediaIndex, specified))
		}
		return textResp(err.Status, err.Message)
	}
	return cacheable(jsonResp(200, a.buildOEmbed(baseURL, post, postType, mediaIndex, specified)), edgeCacheSeconds)
}

func (a *App) publicBaseURL(req *http.Request) string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	host := req.Host
	if host == "" {
		host = "localhost:" + strconv.Itoa(a.cfg.Port)
	}
	proto := firstHeaderValue(req.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
			proto = "http"
		} else {
			proto = "https"
		}
	}
	return proto + "://" + host
}

func (a *App) publicBaseURLFromHost(host string) string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	proto := "https"
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		proto = "http"
	}
	return proto + "://" + host
}

func firstHeaderValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(v, ",", 2)[0])
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
