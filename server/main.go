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

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 {
				switch a.Key {
				case slog.TimeKey:
					return slog.Attr{}
				case slog.LevelKey:
					a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
				}
			}
			return a
		},
	})))

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
func activityJSONResp(status int, body []byte) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "application/activity+json"}, body: body}
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

	a.write(w, a.route(req))
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
		r := resp{status: 200, headers: map[string]string{"Content-Type": f.contentType}, body: f.body}
		if f.immutable {
			r.headers["Cache-Control"] = "public, max-age=31536000, s-maxage=31536000, immutable"
			return r
		}
		return cacheable(r, iconCacheSeconds)
	}
	segments := splitPath(path)
	if (len(segments) == 2 || len(segments) == 3) && segments[0] == "offload" {
		return a.handleOffload(req, segments)
	}

	if len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "statuses" {
		return a.handleMastodonStatus(req, segments[3])
	}
	if len(segments) == 4 && segments[0] == "users" && segments[2] == "statuses" {
		return a.handleActivity(req, segments[1], segments[3])
	}
	if len(segments) == 2 && segments[0] == "users" {
		return a.handleUserAccount(req, segments[1])
	}
	if route := parseEmbedSegments(segments); route != nil {
		pathIndex := -1
		if route.HasIndex {
			pathIndex = route.PathIndex
		}
		return a.handleEmbed(req, route.PostType, route.Shortcode, pathIndex)
	}
	if len(segments) == 1 && validUsername(segments[0]) {
		return a.handleProfile(req, segments[0])
	}
	return resp{status: 404, headers: map[string]string{}}
}

func (a *App) handleUserAccount(req *http.Request, username string) resp {
	baseURL := a.publicBaseURL(req)
	p, err := a.getProfile(username, nil)
	if err != nil {
		return cacheable(activityJSONResp(200, a.buildFallbackAccount(baseURL, username)), errorCacheSeconds(err.Reason))
	}
	return cacheable(activityJSONResp(200, a.buildProfileAccount(baseURL, p)), edgeCacheSeconds)
}

func (a *App) handleActivity(req *http.Request, username, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Username != "" {
		p, err := a.getProfile(sp.Username, nil)
		if err != nil {
			return textResp(err.Status, err.Message)
		}
		return cacheable(activityJSONResp(200, a.buildProfileActivityStatus(baseURL, p)), edgeCacheSeconds)
	}
	post, err := a.getPost(sp.Shortcode, nil)
	if err != nil {
		if isTransient(err.Reason) {
			return activityJSONResp(err.Status, a.buildRateLimitActivityStatus(baseURL, sp.Shortcode))
		}
		return textResp(err.Status, err.Message)
	}
	body := a.buildActivityStatus(baseURL, post, sp.PostType, snowMediaIndex(sp), sp.Specified, sp.Gallery)
	return cacheable(activityJSONResp(200, body), edgeCacheSeconds)
}

func (a *App) handleMastodonStatus(req *http.Request, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Username != "" {
		p, err := a.getProfile(sp.Username, nil)
		if err != nil {
			return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.Message}))
		}
		return cacheable(jsonResp(200, a.buildMastodonProfileStatus(baseURL, p)), edgeCacheSeconds)
	}
	if !validShortcode(sp.Shortcode) {
		return jsonResp(404, jsonBytes(map[string]any{"error": "Record not found"}))
	}
	post, err := a.getPost(sp.Shortcode, nil)
	if err != nil {
		return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.Message}))
	}
	body := a.buildMastodonStatus(baseURL, post, sp.PostType, snowMediaIndex(sp), sp.Specified, sp.Gallery)
	return cacheable(jsonResp(200, body), edgeCacheSeconds)
}

func snowMediaIndex(sp snowPost) int {
	if sp.Specified {
		return sp.MediaIndex
	}
	return -1
}

func (a *App) handleProfile(req *http.Request, username string) resp {
	origin := profileURL(username)
	if !botRE.MatchString(req.Header.Get("User-Agent")) {
		return redirectResp(origin, 307)
	}
	q := parseRequestQuery(req.URL.Query())
	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	p, err := a.getProfile(username, meta)
	if err != nil {
		title, desc := profileErrorCard(err.Reason)
		embed := a.buildStatusEmbedHTML(baseURL, origin, title, desc)
		r := htmlResp(200, embed)
		r.headers["x-og-status"] = strconv.Itoa(err.Status)
		if err.Reason != "" {
			r.headers["x-og-reason"] = err.Reason
		}
		r = cacheable(r, errorCacheSeconds(err.Reason))
		return tagFetch(r, meta)
	}
	return tagFetch(cacheable(htmlResp(200, a.buildProfileEmbedHTML(baseURL, p, q.Gallery)), edgeCacheSeconds), meta)
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
		title, desc := postErrorCard(reason)

		if reason == reasonMediaNotFound {
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
		r = cacheable(r, errorCacheSeconds(err.Reason))
		return tagFetch(r, meta)
	}
	html := a.buildEmbedHTML(baseURL, req.Header.Get("User-Agent"), post, postType, mediaIndex, specified, q.Gallery)
	return tagFetch(cacheable(htmlResp(200, html), edgeCacheSeconds), meta)
}

func (a *App) handleOffload(req *http.Request, segments []string) resp {
	shortcode := segments[1]
	index := 0
	if len(segments) == 3 {
		seg := strings.TrimSuffix(segments[2], ".mp4")
		if n, err := strconv.Atoi(seg); err == nil && n > 0 {
			index = n - 1
		}
	}
	thumbnail := req.URL.Query().Has("thumbnail")
	meta := &fetchMeta{}
	post, err := a.getPost(shortcode, meta)
	if err != nil {
		if isTransient(err.Reason) {
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

func (a *App) publicBaseURL(req *http.Request) string {
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	host := req.Host
	if host == "" {
		host = "localhost:" + strconv.Itoa(a.cfg.Port)
	}
	if proto := firstHeaderValue(req.Header.Get("X-Forwarded-Proto")); proto != "" {
		return proto + "://" + host
	}
	return a.publicBaseURLFromHost(host)
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
