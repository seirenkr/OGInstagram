package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

func main() {

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 {
				switch a.Key {
				case slog.TimeKey:
					return slog.Attr{}
				case slog.MessageKey:
					a.Key = "message"
				case slog.LevelKey:
					a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
				}
			}
			return a
		},
	})))

	cfg := configFromEnv()
	if cfg.BudgetURL == "" {
		slog.Error("invalid configuration", "error", "BUDGET_URL is required")
		os.Exit(1)
	}
	app := newApp(cfg, newSessionPool(cfg))

	slog.Info("container started", "service", serviceName, "version", cfg.Version,
		"proxies", len(app.pool.sessions), "port", cfg.Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handle)

	if err := http.ListenAndServe("0.0.0.0:"+strconv.Itoa(cfg.Port), mux); err != nil {
		slog.Error("server exited", "err", err.Error())
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
func tagFetch(r resp, meta *fetchMeta) resp {
	if meta.fetched {
		r.headers["og-cache"] = "miss"
	} else {
		r.headers["og-cache"] = "hit"
	}
	return r
}

func (a *App) handle(w http.ResponseWriter, req *http.Request) {
	if id := strings.TrimSpace(req.Header.Get("OG-Request-ID")); id != "" && len(id) <= 128 {
		req = req.WithContext(context.WithValue(req.Context(), requestIDKey{}, id))
	}
	a.write(w, a.route(req))
}

type requestIDKey struct{}

func logger(ctx context.Context) *slog.Logger {
	if id, _ := ctx.Value(requestIDKey{}).(string); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}

func (a *App) route(req *http.Request) resp {
	path := req.URL.Path

	if path == "/.well-known/webfinger" {
		return a.handleWebFinger(req)
	}
	segments := splitPath(path)
	if len(segments) == 5 && segments[0] == "offload" && segments[1] == "story" && segments[4] == "avatar" {
		return a.handleStoryOffload(req, segments[2], segments[3], true)
	}
	if len(segments) == 4 && segments[0] == "offload" && segments[1] == "story" {
		return a.handleStoryOffload(req, segments[2], segments[3], false)
	}
	if (len(segments) == 2 || len(segments) == 3) && segments[0] == "offload" {
		return a.handleOffload(req, segments)
	}
	if len(segments) == 3 && segments[0] == "stories" && validUsername(segments[1]) && validStoryID(segments[2]) {
		return a.handleStory(req, segments[1], segments[2])
	}

	if len(segments) == 4 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "statuses" {
		return a.handleMastodonStatus(req, segments[3])
	}
	if len(segments) == 4 && segments[0] == "users" && segments[2] == "statuses" {
		return a.handleActivity(req, segments[1], segments[3])
	}
	if len(segments) == 3 && segments[0] == "users" && validUsername(segments[1]) && (segments[2] == "inbox" || segments[2] == "outbox") {
		return a.handleActivityCollection(req, segments[1], segments[2])
	}
	if len(segments) == 2 && segments[0] == "users" && validUsername(segments[1]) {
		return a.handleUserAccount(req, segments[1])
	}
	if route := parseEmbedSegments(segments); route != nil {
		return a.handleEmbed(req, route.PostType, route.Shortcode, route.PathIndex)
	}
	if len(segments) == 1 && validUsername(segments[0]) {
		return a.handleProfile(req, segments[0])
	}
	return resp{status: 404, headers: map[string]string{}}
}

func (a *App) handleUserAccount(req *http.Request, username string) resp {
	baseURL := a.publicBaseURL(req)
	return cacheable(activityJSONResp(200, a.buildFallbackAccount(baseURL, username)), edgeCacheSeconds)
}

func (a *App) handleWebFinger(req *http.Request) resp {
	if req.Method != http.MethodGet {
		return resp{status: http.StatusMethodNotAllowed, headers: map[string]string{"Allow": "GET"}}
	}
	resource := req.URL.Query().Get("resource")
	account, ok := strings.CutPrefix(resource, "acct:")
	at := strings.LastIndexByte(account, '@')
	if !ok || at <= 0 || at == len(account)-1 {
		return textResp(http.StatusBadRequest, "invalid resource")
	}
	username, host := account[:at], account[at+1:]
	baseURL := a.publicBaseURL(req)
	base, err := url.Parse(baseURL)
	if err != nil || !validUsername(username) || !strings.EqualFold(host, base.Host) {
		return textResp(http.StatusNotFound, "resource not found")
	}
	actor := actorURL(baseURL, username)
	links := []any{}
	rels := req.URL.Query()["rel"]
	if len(rels) == 0 || slices.Contains(rels, "self") {
		links = append(links, map[string]any{"rel": "self", "type": "application/activity+json", "href": actor})
	}
	r := cacheable(jsonResp(http.StatusOK, jsonBytes(map[string]any{
		"subject": "acct:" + username + "@" + base.Host,
		"aliases": []any{actor},
		"links":   links,
	})), edgeCacheSeconds)
	r.headers["Content-Type"] = "application/jrd+json"
	r.headers["Access-Control-Allow-Origin"] = "*"
	return r
}

func (a *App) handleActivityCollection(req *http.Request, username, name string) resp {
	if req.Method != http.MethodGet {
		return resp{status: http.StatusMethodNotAllowed, headers: map[string]string{"Allow": "GET"}}
	}
	id := actorURL(a.publicBaseURL(req), username) + "/" + name
	return cacheable(activityJSONResp(http.StatusOK, emptyOrderedCollection(id)), edgeCacheSeconds)
}

func (a *App) handleActivity(req *http.Request, _, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Story {
		story, err := a.getStory(req.Context(), sp.Username, sp.Shortcode, nil)
		if err != nil {
			return textResp(err.Status, err.Message)
		}
		return cacheable(activityJSONResp(200, a.buildStoryActivityStatus(baseURL, story, sp.Gallery)), edgeCacheSeconds)
	}
	if sp.Username != "" {
		p, err := a.getProfile(req.Context(), sp.Username, nil)
		if err != nil {
			return textResp(err.Status, err.Message)
		}
		return cacheable(activityJSONResp(200, a.buildProfileActivityStatus(baseURL, p)), cdnEdgeSeconds(profileCDNURLs(p)...))
	}
	post, err := a.getPost(req.Context(), sp.Shortcode, nil)
	if err != nil {
		return textResp(err.Status, err.Message)
	}
	body := a.buildActivityStatus(baseURL, post, sp.PostType, snowMediaIndex(sp), sp.Specified, sp.Gallery)
	return cacheable(activityJSONResp(200, body), edgeCacheSeconds)
}

func (a *App) handleMastodonStatus(req *http.Request, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Story {
		story, err := a.getStory(req.Context(), sp.Username, sp.Shortcode, nil)
		if err != nil {
			return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.Message}))
		}
		return cacheable(jsonResp(200, a.buildStoryMastodonStatus(baseURL, story, sp.Gallery)), edgeCacheSeconds)
	}
	if sp.Username != "" {
		p, err := a.getProfile(req.Context(), sp.Username, nil)
		if err != nil {
			return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.Message}))
		}
		return cacheable(jsonResp(200, a.buildMastodonProfileStatus(baseURL, p)), cdnEdgeSeconds(profileCDNURLs(p)...))
	}
	// getPost validates too, but the Mastodon API's spec'd 404 body is
	// {"error":"Record not found"}, so answer with that exact shape here.
	if !validShortcode(sp.Shortcode) {
		return jsonResp(404, jsonBytes(map[string]any{"error": "Record not found"}))
	}
	post, err := a.getPost(req.Context(), sp.Shortcode, nil)
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
	gallery := galleryRequested(req.URL.Query())
	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	p, err := a.getProfile(req.Context(), username, meta)
	if err != nil {
		title, desc := profileErrorCard(err.Reason, supportURL)
		embed := a.buildStatusEmbedHTML(baseURL, origin, title, desc)
		r := htmlResp(200, embed)
		r.headers["og-status"] = strconv.Itoa(err.Status)
		if err.Reason != "" {
			r.headers["og-reason"] = err.Reason
		}
		r = cacheable(r, errorCacheSeconds(err.Reason))
		return tagFetch(r, meta)
	}
	return tagFetch(cacheable(htmlResp(200, a.buildProfileEmbedHTML(baseURL, p, gallery)), cdnEdgeSeconds(profileCDNURLs(p)...)), meta)
}

func (a *App) handleEmbed(req *http.Request, postType, shortcode string, pathIndex int) resp {
	values := req.URL.Query()
	mediaIndex, specified := mediaSelection(values, pathIndex)
	gallery := galleryRequested(values)
	origin := instagramPostURL(postType, shortcode, mediaIndex, specified)

	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	post, err := a.getPost(req.Context(), shortcode, meta)
	if err != nil {
		reason := err.Reason
		title, desc := postErrorCard(reason, supportURL)
		if err.CardTitle != "" {
			reason, title, desc = err.CardReason, err.CardTitle, err.CardDesc
		}
		embed := a.buildStatusEmbedHTML(baseURL, origin, title, desc)
		r := htmlResp(200, embed)
		r.headers["og-status"] = strconv.Itoa(err.Status)
		if reason != "" {
			r.headers["og-reason"] = reason
		}
		r = cacheable(r, errorCacheSeconds(err.Reason))
		return tagFetch(r, meta)
	}
	html := a.buildEmbedHTML(req.Context(), baseURL, req.Header.Get("User-Agent"), post, postType, mediaIndex, specified, gallery)
	return tagFetch(cacheable(htmlResp(200, html), edgeCacheSeconds), meta)
}

func offloadErrorResp(err *AppError, fallbackURL string) resp {
	r := redirectResp(fallbackURL, 302)
	if !isTransient(err.Reason) {
		r = textResp(err.Status, err.Message)
	}
	r.headers["og-status"] = strconv.Itoa(err.Status)
	r.headers["og-reason"] = err.Reason
	return r
}

func allowOffloadFetch(req *http.Request) bool {
	return req.Header.Get("OG-Allow-Offload-Fetch") == "1"
}

func (a *App) handleOffload(req *http.Request, segments []string) resp {
	if username, ok := strings.CutPrefix(segments[1], "@"); ok {
		return a.handleProfileOffload(req, username, segments)
	}
	shortcode := segments[1]
	if !allowOffloadFetch(req) && !a.posts.known(req.Context(), shortcode) {
		return resp{status: 404, headers: map[string]string{}}
	}
	index := 0
	if len(segments) == 3 {
		seg := strings.TrimSuffix(segments[2], ".mp4")
		if n, err := strconv.Atoi(seg); err == nil && n > 0 {
			index = n - 1
		}
	}
	thumbnail := req.URL.Query().Has("thumbnail")
	meta := &fetchMeta{}
	post, err := a.getPost(req.Context(), shortcode, meta)
	if err != nil {
		return tagFetch(offloadErrorResp(err, instagramOrigin+"/p/"+url.PathEscape(shortcode)+"/"), meta)
	}
	target := ""
	if len(segments) == 3 && segments[2] == "avatar" {
		target = post.ProfilePic
	} else {
		att := post.Attachments[mediaIndexFor(post, index)]
		target = att.URL
		if thumbnail && att.Thumbnail != "" {
			target = att.Thumbnail
		}
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(cacheable(redirectResp(target, 302), cdnEdgeSeconds(target)), meta)
}

func (a *App) handleProfileOffload(req *http.Request, username string, segments []string) resp {
	if !allowOffloadFetch(req) && !a.profiles.known(req.Context(), username) {
		return resp{status: 404, headers: map[string]string{}}
	}
	meta := &fetchMeta{}
	p, err := a.getProfile(req.Context(), username, meta)
	if err != nil {
		return tagFetch(offloadErrorResp(err, instagramOrigin+"/"+url.PathEscape(username)+"/"), meta)
	}
	target := p.ProfilePic
	if len(segments) == 3 && segments[2] != "avatar" {
		n, atoiErr := strconv.Atoi(segments[2])
		if atoiErr != nil || n < 1 || n > len(p.RecentMedia) {
			return resp{status: 404, headers: map[string]string{}}
		}
		target = p.RecentMedia[n-1].Thumbnail
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(cacheable(redirectResp(target, 302), cdnEdgeSeconds(target)), meta)
}

func (a *App) publicBaseURL(req *http.Request) string {
	if origin := strings.TrimSpace(req.Header.Get("OG-Public-Origin")); origin != "" {
		return strings.TrimRight(origin, "/")
	}
	if a.cfg.BaseURL != "" {
		return a.cfg.BaseURL
	}
	host := req.Host
	if host == "" {
		host = "localhost:" + strconv.Itoa(a.cfg.Port)
	}
	if proto := strings.TrimSpace(strings.SplitN(req.Header.Get("X-Forwarded-Proto"), ",", 2)[0]); proto != "" {
		return proto + "://" + host
	}
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		return "http://" + host
	}
	return "https://" + host
}
