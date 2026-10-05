package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "check the local server readiness")
	backup := flag.String("backup", "", "write SQLite backups into a new destination directory")
	exhaustBudget := flag.Bool("exhaust-budget", false, "exhaust today's proxy budget after restoring a backup")
	flag.Parse()
	cfg := configFromEnv()
	if *healthcheck {
		client := &http.Client{Timeout: 2 * time.Second}
		r, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", cfg.Port))
		if err != nil {
			os.Exit(1)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	// JSON lines on stderr; the Docker log driver stores and rotates them.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	if err := cfg.validate(); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	store, err := openLocalStore(cfg.DataDir, cfg.BudgetStartDate)
	if err != nil {
		slog.Error("cannot open local storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	cfg.Store = store
	if *backup != "" || *exhaustBudget {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if *exhaustBudget {
			err = store.ExhaustBudget(ctx)
		} else {
			err = store.Backup(ctx, *backup)
		}
		if err != nil {
			slog.Error("storage maintenance failed", "error", err)
			os.Exit(1)
		}
		return
	}
	home, err := loadHomeTemplates(cfg.AssetsDir)
	if err != nil {
		slog.Error("frontend assets unavailable", "error", err)
		os.Exit(1)
	}
	keys := cfg.OffloadSigningKeys
	if keys == "" {
		if keys, err = loadOrCreateOffloadKeys(cfg.DataDir); err != nil {
			slog.Error("cannot create offload signing keys", "error", err)
			os.Exit(1)
		}
	}
	signer, err := parseOffloadSigner(keys)
	if err != nil {
		slog.Error("invalid OFFLOAD_SIGNING_KEYS", "error", err)
		os.Exit(1)
	}
	app := newApp(cfg, newSessionPool(cfg), signer)

	slog.Info("server started", "service", serviceName, "version", cfg.Version,
		"proxies", len(app.pool.sessions), "port", cfg.Port)

	srv := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(cfg.Port),
		Handler:           newGateway(cfg, app, home),
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := serveHTTP(ctx, srv); err != nil {
		slog.Error("server exited", "error", err)
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

// The localized home pages are immutable build output, so they are read once.
func loadHomeTemplates(directory string) (map[string]string, error) {
	pages := make(map[string]string, len(homeLocales))
	for locale := range homeLocales {
		name := filepath.Join(directory, "home", locale+".html")
		body, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("%s: empty home template", name)
		}
		pages[locale] = string(body)
	}
	return pages, nil
}

type resp struct {
	status  int
	headers map[string]string
	body    []byte

	// Read by the gateway for metrics and preview status; never sent to clients.
	originStatus int
	errorType    string
	cacheHit     bool
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

func tagFetch(r resp, meta *fetchMeta) resp {
	r.cacheHit = !meta.fetched
	return r
}

type requestIDKey struct{}

func logger(ctx context.Context) *slog.Logger {
	if id, _ := ctx.Value(requestIDKey{}).(string); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}

func (a *App) handleWebFinger(req *http.Request) resp {
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
	r := jsonResp(http.StatusOK, jsonBytes(map[string]any{
		"subject": "acct:" + username + "@" + base.Host,
		"aliases": []any{actor},
		"links":   links,
	}))
	r.headers["Content-Type"] = "application/jrd+json"
	r.headers["Access-Control-Allow-Origin"] = "*"
	return r
}

func (a *App) handleActivityCollection(req *http.Request, username, name string) resp {
	id := actorURL(a.publicBaseURL(req), username) + "/" + name
	return activityJSONResp(http.StatusOK, emptyOrderedCollection(id))
}

func (a *App) handleActivity(req *http.Request, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Story {
		story, err := a.getStory(req.Context(), sp.Username, sp.Shortcode, nil)
		if err != nil {
			return textResp(err.Status, err.PublicMessage)
		}
		return activityJSONResp(200, a.buildStoryActivityStatus(baseURL, story, sp.Gallery))
	}
	if sp.Username != "" {
		p, err := a.getProfile(req.Context(), sp.Username, nil)
		if err != nil {
			return textResp(err.Status, err.PublicMessage)
		}
		return activityJSONResp(200, a.buildProfileActivityStatus(baseURL, p))
	}
	post, err := a.getPost(req.Context(), sp.Shortcode, nil)
	if err != nil {
		return textResp(err.Status, err.PublicMessage)
	}
	body := a.buildActivityStatus(baseURL, post, sp.PostType, sp.MediaIndex, sp.Specified, sp.Gallery)
	return activityJSONResp(200, body)
}

func (a *App) handleMastodonStatus(req *http.Request, code string) resp {
	sp := parseStatusSnowcode(code)
	baseURL := a.publicBaseURL(req)
	if sp.Story {
		story, err := a.getStory(req.Context(), sp.Username, sp.Shortcode, nil)
		if err != nil {
			return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.PublicMessage}))
		}
		return jsonResp(200, a.buildStoryMastodonStatus(baseURL, story, sp.Gallery))
	}
	if sp.Username != "" {
		p, err := a.getProfile(req.Context(), sp.Username, nil)
		if err != nil {
			return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.PublicMessage}))
		}
		return jsonResp(200, a.buildMastodonProfileStatus(baseURL, p))
	}

	if !validShortcode(sp.Shortcode) {
		return jsonResp(404, jsonBytes(map[string]any{"error": "Record not found"}))
	}
	post, err := a.getPost(req.Context(), sp.Shortcode, nil)
	if err != nil {
		return jsonResp(err.Status, jsonBytes(map[string]any{"error": err.PublicMessage}))
	}
	body := a.buildMastodonStatus(baseURL, post, sp.PostType, sp.MediaIndex, sp.Specified, sp.Gallery)
	return jsonResp(200, body)
}

func (a *App) handleProfile(req *http.Request, username string, gallery bool) resp {
	origin := profileURL(username)
	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	p, err := a.getProfile(req.Context(), username, meta)
	if err != nil {
		title, desc := errorCard("profile", err.Code)
		return a.errorCardResp(baseURL, origin, title, desc, err.Code, err, meta)
	}
	variant := discordRequestVariant(req)
	html := a.buildProfileEmbedHTML(baseURL, p, gallery, variant)
	return tagFetch(discordEmbedResponse(req, html, variant), meta)
}

func (a *App) handlePost(req *http.Request, postType, shortcode string, mediaIndex int, specified, gallery bool) resp {
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
	variant := discordRequestVariant(req)
	html := a.buildEmbedHTML(baseURL, post, postType, mediaIndex, specified, gallery, variant)
	return tagFetch(discordEmbedResponse(req, html, variant), meta)
}

func (a *App) errorCardResp(baseURL, origin, title, desc, headerCode string, err *AppError, meta *fetchMeta) resp {
	r := htmlResp(200, a.buildStatusEmbedHTML(baseURL, origin, title, desc))
	r.originStatus, r.errorType = err.Status, headerCode
	return tagFetch(r, meta)
}

func offloadErrorResp(err *AppError, fallbackURL string) resp {
	r := redirectResp(fallbackURL, 302)
	if !isTransient(err.Code) {
		r = textResp(err.Status, err.PublicMessage)
	}
	r.originStatus, r.errorType = err.Status, err.Code
	return r
}

func invalidOffloadResp() resp {
	return resp{status: http.StatusNotFound, headers: map[string]string{}}
}

// Offload handlers trust their arguments: canonicalOffloadPath or the d. host
// route has already validated and bounded them.
func (a *App) handleOffload(req *http.Request, shortcode string, index int, avatar bool) resp {
	meta := &fetchMeta{}
	post, err := a.getPost(req.Context(), shortcode, meta)
	if err != nil {
		return tagFetch(offloadErrorResp(err, instagramOrigin+"/p/"+url.PathEscape(shortcode)+"/"), meta)
	}
	target := post.ProfilePic
	if !avatar {
		media := post.Attachments[mediaIndexFor(post, index)]
		target = media.URL
		if req.URL.Query().Has("thumbnail") && media.Thumbnail != "" {
			target = media.Thumbnail
		}
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(redirectResp(target, 302), meta)
}

// n selects the nth recent media item; 0 selects the profile picture.
func (a *App) handleProfileOffload(req *http.Request, username string, n int) resp {
	meta := &fetchMeta{}
	p, err := a.getProfile(req.Context(), username, meta)
	if err != nil {
		return tagFetch(offloadErrorResp(err, instagramOrigin+"/"+url.PathEscape(username)+"/"), meta)
	}
	target := p.ProfilePic
	if n > 0 {
		if n > len(p.RecentMedia) {
			return invalidOffloadResp()
		}
		target = p.RecentMedia[n-1].Thumbnail
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(redirectResp(target, 302), meta)
}

// The gateway has already matched Host against ALLOWED_HOSTS (or development
// localhost), so it is the request's public origin.
func (a *App) publicBaseURL(req *http.Request) string {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "http://" + req.Host
	}
	return "https://" + req.Host
}
