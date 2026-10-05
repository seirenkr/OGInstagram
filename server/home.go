package main

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var homeLocales = map[string]bool{
	"en": true, "es": true, "fr": true, "ja": true, "ko": true,
	"pt": true, "zh-hans": true, "zh-hant": true,
}

var homeQualityPattern = regexp.MustCompile(`^(?:0(?:\.[0-9]{0,3})?|1(?:\.0{0,3})?)$`)

func (g *Gateway) serveHome(w http.ResponseWriter, r *http.Request) {
	base, _ := url.Parse(g.cfg.BaseURL) // validated at startup
	if !g.cfg.Development && !strings.EqualFold(r.Host, base.Host) {
		target := url.URL{Scheme: base.Scheme, Host: base.Host, Path: "/"}
		if language := r.URL.Query().Get("hl"); language != "" {
			target.RawQuery = url.Values{"hl": {language}}.Encode()
		}
		http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
		return
	}
	forced := strings.ToLower(r.URL.Query().Get("hl"))
	if !homeLocales[forced] {
		forced = ""
	}
	cookieLocale := ""
	if cookie, err := r.Cookie("hl"); err == nil && homeLocales[cookie.Value] {
		cookieLocale = cookie.Value
	}
	locale := cmp.Or(forced, cookieLocale, resolveHomeLocale(r.Header.Get("Accept-Language")))
	nonce := rand.Text()
	origin := base.Scheme + "://" + base.Host
	canonical := origin + "/"
	if forced != "" {
		canonical += "?hl=" + forced
	}
	// Host, key and version occur inside JSON strings. Use JSON escaping here
	// rather than accepting configuration as executable markup.
	page := strings.NewReplacer(
		"__OG_CANONICAL__", html.EscapeString(canonical),
		"__OG_BASE__", html.EscapeString(origin),
		"__OG_HOST__", homeJSONString(base.Host),
		"__OG_TURNSTILE_SITE_KEY__", homeJSONString(g.cfg.TurnstileSiteKey),
		"__OG_CSP_NONCE__", nonce,
		"__OG_VERSION__", homeJSONString(g.cfg.Version),
	).Replace(g.home[locale])
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Every page carries a fresh nonce and can depend on the language cookie.
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Accept-Language, Cookie")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'nonce-"+nonce+"' https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; img-src 'self' data: https://*.cdninstagram.com https://*.fbcdn.net; font-src 'self' https://cdn.jsdelivr.net; connect-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	if forced != "" && forced != cookieLocale {
		http.SetCookie(w, &http.Cookie{Name: "hl", Value: forced, Path: "/", MaxAge: 31536000, SameSite: http.SameSiteLaxMode, Secure: base.Scheme == "https"})
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, page)
	}
}

func homeJSONString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded[1 : len(encoded)-1])
}

func setPublicSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
	h.Set("X-Frame-Options", "DENY")
}

func resolveHomeLocale(acceptLanguage string) string {
	type languageRange struct {
		tag     string
		quality float64
	}
	var ranges []languageRange
	for _, part := range strings.Split(acceptLanguage, ",") {
		parameters := strings.Split(strings.TrimSpace(part), ";")
		tag, quality := strings.ToLower(parameters[0]), 1.0
		for _, parameter := range parameters[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			value = strings.TrimSpace(value)
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || !homeQualityPattern.MatchString(value) {
				quality = 0
			} else {
				quality = parsed
			}
		}
		if tag != "" && tag != "*" && quality > 0 {
			ranges = append(ranges, languageRange{tag, quality})
		}
	}
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].quality > ranges[j].quality })
	for _, candidate := range ranges {
		tag := candidate.tag
		if tag == "zh" || strings.HasPrefix(tag, "zh-") {
			if strings.Contains(tag, "hant") || strings.HasSuffix(tag, "-tw") || strings.HasSuffix(tag, "-hk") || strings.HasSuffix(tag, "-mo") {
				return "zh-hant"
			}
			return "zh-hans"
		}
		primary, _, _ := strings.Cut(tag, "-")
		if homeLocales[primary] {
			return primary
		}
	}
	return "en"
}

// serveAsset serves only published build artifacts. Templates and build control
// files remain private, and directories never produce a listing or an index.
func (g *Gateway) serveAsset(w http.ResponseWriter, r *http.Request) bool {
	file := strings.TrimPrefix(r.URL.Path, "/")
	if file == ".well-known/webfinger" {
		return false
	}
	immutable := strings.HasPrefix(file, "assets/") || strings.HasPrefix(file, "preview/") || strings.HasPrefix(file, "twemoji/") || file == "PPMori-Regular.woff2" || file == "PPMori-Semibold.woff2"
	allowed := immutable || file == "default-avatar.jpg" || file == "favicon-192.png" || file == "favicon-64.png"
	// Root files crawlers probe would otherwise parse as Instagram usernames.
	reserved := file == "assets" || file == "preview" || file == "twemoji" || file == "home" || strings.HasPrefix(file, "home/") || strings.HasPrefix(file, ".") ||
		file == "robots.txt" || file == "favicon.ico" || file == "sitemap.xml"
	if !allowed && !reserved {
		return false
	}
	if !allowMethod(w, r, http.MethodGet, http.MethodHead) {
		return true
	}
	badPath := !allowed || strings.Contains(file, "\\") || strings.ContainsRune(file, 0)
	for _, segment := range strings.Split(file, "/") {
		if segment == "" || strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_") {
			badPath = true
		}
	}
	if badPath {
		http.NotFound(w, r)
		return true
	}
	root, err := os.OpenRoot(g.cfg.AssetsDir)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer root.Close()
	f, err := root.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return true
	}
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	return true
}
