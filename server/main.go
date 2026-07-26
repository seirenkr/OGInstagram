package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	if cfg.CacheURL == "" {
		slog.Error("invalid configuration", "event", "configuration_invalid", "error.type", "missing_cache_url",
			"exception.message", "CACHE_URL is required")
		os.Exit(1)
	}
	if cfg.BudgetURL == "" {
		slog.Error("invalid configuration", "event", "configuration_invalid", "error.type", "missing_budget_url",
			"exception.message", "BUDGET_URL is required")
		os.Exit(1)
	}
	signer, err := parseOffloadSigner(cfg.OffloadSigningKeys)
	if err != nil {
		slog.Error("invalid configuration", "event", "configuration_invalid", "error.type", "invalid_offload_signing_keys",
			"exception.message", err.Error())
		os.Exit(1)
	}
	app := newApp(cfg, newSessionPool(cfg), signer)

	slog.Info("container started", "service", serviceName, "version", cfg.Version,
		"proxies", len(app.pool.sessions), "port", cfg.Port)

	srv := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(cfg.Port),
		Handler:           http.HandlerFunc(app.handle),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := serveHTTP(ctx, srv); err != nil {
		slog.Error("server exited", "event", "server_exit_failed", "error.type", "server_error",
			"exception.message", err.Error())
		os.Exit(1)
	}
	slog.Info("server stopped")
}

func serveHTTP(ctx context.Context, srv *http.Server) error {
	errC := make(chan error, 1)
	go func() {
		errC <- srv.ListenAndServe()
	}()

	select {
	case err := <-errC:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = srv.Close()
	}
	serveErr := <-errC
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return shutdownErr
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
func problemResp(status int, detail string) resp {
	return resp{
		status: status,
		headers: map[string]string{
			"Content-Type":           "application/problem+json",
			"X-Content-Type-Options": "nosniff",
		},
		body: jsonBytes(map[string]any{
			"type":   "about:blank",
			"title":  http.StatusText(status),
			"status": status,
			"detail": detail,
		}),
	}
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
	effectiveStatus := r.status
	if status, err := strconv.Atoi(r.headers[headerOriginStatus]); err == nil {
		effectiveStatus = status
	}
	if effectiveStatus == 499 || effectiveStatus == http.StatusRequestTimeout ||
		effectiveStatus == http.StatusTooEarly || effectiveStatus == http.StatusTooManyRequests ||
		effectiveStatus >= 500 {
		return uncacheable(r)
	}

	r.headers["Cache-Control"] = "public, max-age=0"
	cdnControl := "public, max-age=" + strconv.Itoa(seconds)
	if r.status == http.StatusOK && effectiveStatus < 400 {
		cdnControl += ", stale-while-revalidate=" + strconv.Itoa(edgeStaleWhileRevalidateSeconds) +
			", stale-if-error=" + strconv.Itoa(edgeStaleIfErrorSeconds)
	} else {
		cdnControl += ", stale-if-error=0"
	}
	r.headers["Cloudflare-CDN-Cache-Control"] = cdnControl
	return r
}

func uncacheable(r resp) resp {
	r.headers["Cache-Control"] = "no-store"
	delete(r.headers, "Cloudflare-CDN-Cache-Control")
	return r
}
func tagFetch(r resp, meta *fetchMeta) resp {
	if meta.fetched {
		r.headers[headerCacheStatus] = "miss"
	} else {
		r.headers[headerCacheStatus] = "hit"
	}
	return r
}

func (a *App) handle(w http.ResponseWriter, req *http.Request) {
	if id := strings.TrimSpace(req.Header.Get(headerRequestID)); id != "" && len(id) <= 128 {
		req = req.WithContext(context.WithValue(req.Context(), requestIDKey{}, id))
	}
	ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	started := time.Now()
	r := a.route(req)

	r.headers[headerOriginDuration] = strconv.FormatInt(time.Since(started).Milliseconds(), 10)
	a.write(w, r)
}

type requestIDKey struct{}

func logger(ctx context.Context) *slog.Logger {
	if id, _ := ctx.Value(requestIDKey{}).(string); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}

func (a *App) route(req *http.Request) resp {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return uncacheable(resp{
			status:  http.StatusMethodNotAllowed,
			headers: map[string]string{"Allow": "GET, HEAD"},
		})
	}
	path := req.URL.Path

	if path == "/.well-known/webfinger" {
		return a.handleWebFinger(req)
	}
	segments := splitPath(path)
	if len(segments) == 5 && segments[0] == "offload" && segments[1] == "story" &&
		validUsername(segments[2]) && validStoryID(segments[3]) && segments[4] == "avatar" {
		return a.handleStoryOffload(req, segments[2], segments[3], true)
	}
	if len(segments) == 4 && segments[0] == "offload" && segments[1] == "story" &&
		validUsername(segments[2]) && validStoryID(segments[3]) {
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
		return a.handlePost(req, route.PostType, route.Shortcode, route.PathIndex)
	}
	if len(segments) == 1 && validUsername(segments[0]) {
		return a.handleProfile(req, segments[0])
	}
	return uncacheable(resp{status: 404, headers: map[string]string{}})
}

func (a *App) handleUserAccount(req *http.Request, username string) resp {
	baseURL := a.publicBaseURL(req)
	return cacheable(activityJSONResp(200, a.buildFallbackAccount(baseURL, username)), edgeCacheSeconds)
}

func (a *App) handleWebFinger(req *http.Request) resp {
	resource := req.URL.Query().Get("resource")
	account, ok := strings.CutPrefix(resource, "acct:")
	at := strings.LastIndexByte(account, '@')
	if !ok || at <= 0 || at == len(account)-1 {
		return uncacheable(textResp(http.StatusBadRequest, "invalid resource"))
	}
	username, host := account[:at], account[at+1:]
	baseURL := a.publicBaseURL(req)
	base, err := url.Parse(baseURL)
	if err != nil || !validUsername(username) || !strings.EqualFold(host, base.Host) {
		return uncacheable(textResp(http.StatusNotFound, "resource not found"))
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
	id := actorURL(a.publicBaseURL(req), username) + "/" + name
	return cacheable(activityJSONResp(http.StatusOK, emptyOrderedCollection(id)), edgeCacheSeconds)
}

func (a *App) handleActivity(req *http.Request, _, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Story {
		story, err := a.getStory(req.Context(), sp.Username, sp.Shortcode, nil)
		if err != nil {
			return cacheable(textResp(err.Status, err.PublicMessage), errorCacheSeconds(err.Code))
		}
		return cacheable(activityJSONResp(200, a.buildStoryActivityStatus(baseURL, story, sp.Gallery)), edgeCacheSeconds)
	}
	if sp.Username != "" {
		p, err := a.getProfile(req.Context(), sp.Username, nil)
		if err != nil {
			return cacheable(textResp(err.Status, err.PublicMessage), errorCacheSeconds(err.Code))
		}
		return cacheable(activityJSONResp(200, a.buildProfileActivityStatus(baseURL, p)), cdnEdgeSeconds(profileCDNURLs(p)...))
	}
	post, err := a.getPost(req.Context(), sp.Shortcode, nil)
	if err != nil {
		return cacheable(textResp(err.Status, err.PublicMessage), errorCacheSeconds(err.Code))
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
			return cacheable(jsonResp(err.Status, jsonBytes(map[string]any{"error": err.PublicMessage})), errorCacheSeconds(err.Code))
		}
		return cacheable(jsonResp(200, a.buildStoryMastodonStatus(baseURL, story, sp.Gallery)), edgeCacheSeconds)
	}
	if sp.Username != "" {
		p, err := a.getProfile(req.Context(), sp.Username, nil)
		if err != nil {
			return cacheable(jsonResp(err.Status, jsonBytes(map[string]any{"error": err.PublicMessage})), errorCacheSeconds(err.Code))
		}
		return cacheable(jsonResp(200, a.buildMastodonProfileStatus(baseURL, p)), cdnEdgeSeconds(profileCDNURLs(p)...))
	}

	if !validShortcode(sp.Shortcode) {
		return uncacheable(jsonResp(404, jsonBytes(map[string]any{"error": "Record not found"})))
	}
	post, err := a.getPost(req.Context(), sp.Shortcode, nil)
	if err != nil {
		return cacheable(jsonResp(err.Status, jsonBytes(map[string]any{"error": err.PublicMessage})), errorCacheSeconds(err.Code))
	}
	body := a.buildMastodonStatus(baseURL, post, sp.PostType, snowMediaIndex(sp), sp.Specified, sp.Gallery)
	return cacheable(jsonResp(200, body), edgeCacheSeconds)
}

func snowMediaIndex(sp snowcodePost) int {
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
		title, desc := errorCard("profile", err.Code)
		return a.errorCardResp(baseURL, origin, title, desc, err.Code, err, meta)
	}
	return tagFetch(cacheable(htmlResp(200, a.buildProfileEmbedHTML(baseURL, p, gallery)), cdnEdgeSeconds(profileCDNURLs(p)...)), meta)
}

func (a *App) handlePost(req *http.Request, postType, shortcode string, pathIndex int) resp {
	values := req.URL.Query()
	mediaIndex, specified := mediaSelection(values, pathIndex)
	gallery := galleryRequested(values)
	origin := instagramPostURL(postType, shortcode, mediaIndex, specified)

	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	post, err := a.getPost(req.Context(), shortcode, meta)
	if err != nil {
		errorCode := err.Code
		title, desc := errorCard("post", errorCode)
		if err.CardTitle != "" {
			errorCode, title, desc = err.CardCode, err.CardTitle, err.CardDesc
		}
		return a.errorCardResp(baseURL, origin, title, desc, errorCode, err, meta)
	}
	html := a.buildEmbedHTML(baseURL, post, postType, mediaIndex, specified, gallery)
	return tagFetch(cacheable(htmlResp(200, html), edgeCacheSeconds), meta)
}

func (a *App) errorCardResp(baseURL, origin, title, desc, headerCode string, err *AppError, meta *fetchMeta) resp {
	r := htmlResp(200, a.buildStatusEmbedHTML(baseURL, origin, title, desc))
	r.headers[headerOriginStatus] = strconv.Itoa(err.Status)
	if headerCode != "" {
		r.headers[headerErrorType] = headerCode
	}
	return tagFetch(cacheable(r, errorCacheSeconds(err.Code)), meta)
}

func offloadErrorResp(err *AppError, fallbackURL string) resp {
	r := redirectResp(fallbackURL, 302)
	if !isTransient(err.Code) {
		r = textResp(err.Status, err.PublicMessage)
	}
	r.headers[headerOriginStatus] = strconv.Itoa(err.Status)
	r.headers[headerErrorType] = err.Code
	return cacheable(r, errorCacheSeconds(err.Code))
}

func allowOffloadFetch(req *http.Request) bool {
	return req.Header.Get(headerAllowOriginFetch) == "1"
}

func invalidOffloadResp() resp {
	return uncacheable(resp{status: http.StatusNotFound, headers: map[string]string{}})
}

func offloadMediaIndex(segment string) (int, bool) {
	raw := strings.TrimSuffix(segment, ".mp4")
	n, ok := parseCanonicalDecimal(raw)
	if !ok || n < 1 || n > maxCachedMediaItems {
		return 0, false
	}
	return n - 1, true
}

func (a *App) handleOffload(req *http.Request, segments []string) resp {
	if username, ok := strings.CutPrefix(segments[1], "@"); ok {
		return a.handleProfileOffload(req, username, segments)
	}
	shortcode := segments[1]
	if !validShortcode(shortcode) {
		return invalidOffloadResp()
	}
	index := 0
	if len(segments) == 3 && segments[2] != "avatar" {
		var ok bool
		index, ok = offloadMediaIndex(segments[2])
		if !ok {
			return invalidOffloadResp()
		}
	}
	if !allowOffloadFetch(req) {
		return invalidOffloadResp()
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
		media := post.Attachments[mediaIndexFor(post, index)]
		target = media.URL
		if thumbnail && media.Thumbnail != "" {
			target = media.Thumbnail
		}
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(cacheable(redirectResp(target, 302), cdnEdgeSeconds(target)), meta)
}

func (a *App) handleProfileOffload(req *http.Request, username string, segments []string) resp {
	if !validUsername(username) {
		return invalidOffloadResp()
	}
	username = strings.ToLower(username)
	if len(segments) == 3 && segments[2] != "avatar" {
		if n, ok := parseCanonicalDecimal(segments[2]); !ok || n < 1 || n > profileGalleryMax {
			return invalidOffloadResp()
		}
	}
	if !allowOffloadFetch(req) {
		return invalidOffloadResp()
	}
	meta := &fetchMeta{}
	p, err := a.getProfile(req.Context(), username, meta)
	if err != nil {
		return tagFetch(offloadErrorResp(err, instagramOrigin+"/"+url.PathEscape(username)+"/"), meta)
	}
	target := p.ProfilePic
	if len(segments) == 3 && segments[2] != "avatar" {
		n, _ := parseCanonicalDecimal(segments[2])
		if n > len(p.RecentMedia) {
			return invalidOffloadResp()
		}
		target = p.RecentMedia[n-1].Thumbnail
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(cacheable(redirectResp(target, 302), cdnEdgeSeconds(target)), meta)
}

func (a *App) publicBaseURL(req *http.Request) string {
	if origin := strings.TrimSpace(req.Header.Get(headerPublicOrigin)); origin != "" {
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
