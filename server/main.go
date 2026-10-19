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
	store, err := openLocalStore(cfg.DataDir)
	if err != nil {
		slog.Error("cannot open local storage", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	cfg.Store = store
	if *backup != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		err = store.Backup(ctx, *backup)
		if err == nil {
			err = backupOffloadKeys(cfg.DataDir, *backup)
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
	keys, keySource := cfg.OffloadSigningKeys, "env"
	if keys == "" {
		keySource = "file"
		if keys, err = loadOrCreateOffloadKeys(cfg.DataDir); err != nil {
			slog.Error("cannot load offload signing keys", "source", keySource, "error", err)
			os.Exit(1)
		}
	}
	signer, err := parseOffloadSigner(keys)
	if err != nil {
		slog.Error("invalid offload signing keys", "source", keySource, "error", err)
		os.Exit(1)
	}
	app := newApp(cfg, newSessionPool(cfg), signer)

	slog.Info("server starting", "service", serviceName, "version", cfg.Version,
		"proxies", len(app.pool.sessions), "port", cfg.Port,
		"offload_keys", keySource, "offload_kid", signer.active)

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
	// stop restores default handling, so a second signal ends a stuck drain.
	go func() { <-ctx.Done(); stop() }()
	if app.helper != nil {
		// Close Chrome at the signal, not after the drain: the other slot is starting.
		context.AfterFunc(ctx, app.helper.close)
		defer app.helper.close()
	}
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
	// The CDN URL whose bytes an offload 200 carries; it bounds the edge TTL.
	mediaURL string
}

func htmlResp(status int, body string) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "text/html; charset=utf-8"}, body: []byte(body)}
}
func jsonResp(status int, body []byte) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "application/json"}, body: body}
}
func problemResp(status int, detail string) resp {
	return resp{status: status, headers: map[string]string{"Content-Type": "application/problem+json"},
		body: jsonBytes(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})}
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

type publicOriginKey struct{}

func logger(ctx context.Context) *slog.Logger {
	if id, _ := ctx.Value(requestIDKey{}).(string); id != "" {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}

// handleWebFinger answers RFC 7033 queries for actors, named by acct: URI or by
// their actor id (SWICG ActivityPub Discovery, "WebFinger").
func (a *App) handleWebFinger(req *http.Request) resp {
	r := a.webFinger(req)
	r.headers["Access-Control-Allow-Origin"] = "*"
	return r
}

func (a *App) webFinger(req *http.Request) resp {
	resource := req.URL.Query().Get("resource")
	origin := apOrigin(a.publicBaseURL(req))
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	username := ""
	if account, ok := strings.CutPrefix(resource, "acct:"); ok {
		at := strings.LastIndexByte(account, '@')
		if at <= 0 || at == len(account)-1 {
			return textResp(http.StatusBadRequest, "invalid resource")
		}
		if strings.EqualFold(account[at+1:], host) {
			username = account[:at]
		}
	} else if u, err := url.Parse(resource); err != nil || !u.IsAbs() {
		return textResp(http.StatusBadRequest, "invalid resource")
	} else if strings.EqualFold(u.Scheme+"://"+u.Host, origin) && u.RawQuery == "" {
		username = strings.TrimPrefix(u.Path, "/")
	}
	if !validUsername(username) {
		return textResp(http.StatusNotFound, "resource not found")
	}
	actor := actorID(origin, username)
	links := []any{}
	rels := req.URL.Query()["rel"]
	for _, link := range []map[string]any{
		{"rel": "self", "type": asMediaType, "href": actor},
		{"rel": "http://webfinger.net/rel/profile-page", "type": "text/html", "href": actor},
		{"rel": "alternate", "type": "text/html", "href": actor},
	} {
		if len(rels) == 0 || slices.Contains(rels, link["rel"].(string)) {
			links = append(links, link)
		}
	}
	r := jsonResp(http.StatusOK, jsonBytes(map[string]any{
		"subject": "acct:" + strings.ToLower(username) + "@" + host,
		"aliases": []any{actor},
		"links":   links,
	}))
	r.headers["Content-Type"] = "application/jrd+json"
	return r
}

func (a *App) handleActivityCollection(req *http.Request, username, name string) resp {
	actor := actorID(a.publicBaseURL(req), username)
	return resp{status: http.StatusOK, headers: map[string]string{"Content-Type": asMediaType}, body: emptyOrderedCollection(actor+"/"+name, actor)}
}

func (a *App) handleProfile(req *http.Request, username string, gallery bool) resp {
	origin := profileURL(username)
	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	p, err := a.getProfile(req.Context(), username, meta)
	activity := wantsActivity(req)
	if err != nil {
		if activity {
			return tagFetch(activityErrorResp(err), meta)
		}
		title, desc := errorCard("profile", err.Code)
		return a.errorCardResp(baseURL, origin, title, desc, err.Code, err, meta)
	}
	id := actorID(baseURL, p.Username)
	if activity {
		return tagFetch(activityResp(a.profileActor(baseURL, p, gallery), id, ""), meta)
	}
	return tagFetch(cardResp(a.buildProfileEmbedHTML(baseURL, p, gallery, discordSupport(req)), id, ""), meta)
}

func (a *App) handlePost(req *http.Request, postType, shortcode string, mediaIndex int, specified, gallery bool) resp {
	origin := instagramPostURL(postType, shortcode, mediaIndex, specified)

	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	post, err := a.getPost(req.Context(), shortcode, meta)
	activity := wantsActivity(req)
	if err != nil {
		if activity {
			return tagFetch(activityErrorResp(err), meta)
		}
		errorCode := err.Code
		title, desc := errorCard("post", errorCode)
		if err.CardTitle != "" {
			errorCode, title, desc = err.CardCode, err.CardTitle, err.CardDesc
		}
		return a.errorCardResp(baseURL, origin, title, desc, errorCode, err, meta)
	}
	id, actor := postRefs(baseURL, post, postType, mediaIndex, specified)
	if activity {
		return tagFetch(activityResp(a.postNote(baseURL, post, postType, mediaIndex, specified, gallery), id, actor), meta)
	}
	return tagFetch(partialResp(cardResp(a.buildEmbedHTML(baseURL, post, postType, mediaIndex, specified, gallery, discordSupport(req)), id, actor), post), meta)
}

// A full post replaces a partial one, so like an error fallback its responses
// carry an originStatus that keeps them out of the edge cache; 203 still
// counts as resolved.
func partialResp(r resp, post Post) resp {
	if post.Partial && r.originStatus == 0 {
		r.originStatus = http.StatusNonAuthoritativeInfo
	}
	return r
}

func (a *App) errorCardResp(baseURL, origin, title, desc, headerCode string, err *AppError, meta *fetchMeta) resp {
	r := cardResp(a.buildStatusEmbedHTML(baseURL, origin, title, desc), "", "")
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
	fallback := instagramOrigin + "/p/" + url.PathEscape(shortcode) + "/"
	var post Post
	var err *AppError
	// A partial card's media need not wait out the flight that found it.
	og, partial := a.ogPosts.localGet(shortcode)
	if _, cached := a.posts.localGet(shortcode); partial && !cached {
		post = og.value
	} else if post, err = a.getPost(req.Context(), shortcode, meta); err != nil {
		return tagFetch(offloadErrorResp(err, fallback), meta)
	}
	// The cover is item 1 only.
	if post.Partial && !avatar && index > 0 {
		return tagFetch(partialResp(redirectResp(fallback, http.StatusFound), post), meta)
	}
	target := post.ProfilePic
	if !avatar {
		media := post.Attachments[mediaIndexFor(post, index)]
		target = media.URL
		if req.URL.Query().Has("thumbnail") {
			target = media.Thumbnail
		}
	}
	return tagFetch(partialResp(a.offloadResp(req, target, avatar), post), meta)
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
	return tagFetch(a.offloadResp(req, target, n == 0), meta)
}

// Gateway.ServeHTTP stores the origin it validated; the host rule below is
// reached only by handler tests that bypass the gateway.
func (a *App) publicBaseURL(req *http.Request) string {
	if origin, _ := req.Context().Value(publicOriginKey{}).(string); origin != "" {
		return origin
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "http://" + req.Host
	}
	return "https://" + req.Host
}
