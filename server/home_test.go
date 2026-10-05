package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testHomeGateway(t *testing.T) *Gateway {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "home"), 0700); err != nil {
		t.Fatal(err)
	}
	for locale := range homeLocales {
		body := "<html lang=\"" + locale + "\"><link rel=\"canonical\" href=\"__OG_CANONICAL__\"><script nonce=\"__OG_CSP_NONCE__\"></script><script id=\"app-data\" type=\"application/json\">{\"host\":\"__OG_HOST__\",\"version\":\"__OG_VERSION__\",\"key\":\"__OG_TURNSTILE_SITE_KEY__\"}</script><img src=\"__OG_BASE__/favicon-192.png\"></html>"
		if err := os.WriteFile(filepath.Join(dir, "home", locale+".html"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	home, err := loadHomeTemplates(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Gateway{cfg: Config{AssetsDir: dir, BaseURL: "https://oginstagram.com", Version: "test", TurnstileSiteKey: "site-key"}, home: home}
}

func TestHomeLocalePreferenceAndNonce(t *testing.T) {
	g := testHomeGateway(t)
	request := httptest.NewRequest(http.MethodGet, "https://oginstagram.com/?hl=ZH-HANT", nil)
	request.Header.Set("Cookie", "hl=ko")
	request.Header.Set("Accept-Language", "ja")
	response := httptest.NewRecorder()
	g.serveHome(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "lang=\"zh-hant\"") {
		t.Fatalf("wrong localized page: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "href=\"https://oginstagram.com/?hl=zh-hant\"") {
		t.Fatal("forced locale canonical missing")
	}
	if strings.Contains(response.Body.String(), "__OG_") {
		t.Fatal("runtime placeholders were exposed")
	}
	nonce := regexp.MustCompile("nonce=\"([A-Z2-7]{26})\"").FindStringSubmatch(response.Body.String())
	if len(nonce) != 2 || !strings.Contains(response.Header().Get("Content-Security-Policy"), "'nonce-"+nonce[1]+"'") {
		t.Fatal("CSP does not match inline nonce")
	}
	if response.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatal("localized nonce page must not enter shared cache")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "zh-hant" || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("incorrect language cookie: %v", cookies)
	}
	second := httptest.NewRecorder()
	g.serveHome(second, request)
	if second.Header().Get("Content-Security-Policy") == response.Header().Get("Content-Security-Policy") {
		t.Fatal("nonce was reused")
	}
}

func TestHomeAcceptLanguageAndCookie(t *testing.T) {
	for _, tc := range []struct{ header, locale string }{
		{"ko;q=0.1,en-US;q=0.8", "en"},
		{"de;q=1,zh-TW;q=0.9", "zh-hant"},
		{"zh-CN", "zh-hans"},
		{"fr;q=0,pt-BR;q=0.5", "pt"},
		{"ja;q=1.001,es;q=0.6", "es"},
		{"ja;q=0.1234,ko;q=0.2", "ko"},
		{"*;q=1,fr;q=0.2", "fr"},
		{"invalid", "en"},
		{"es;q=0.5,fr;q=0.5", "es"},
	} {
		if got := resolveHomeLocale(tc.header); got != tc.locale {
			t.Errorf("%q: got %s want %s", tc.header, got, tc.locale)
		}
	}
	g := testHomeGateway(t)
	request := httptest.NewRequest(http.MethodGet, "https://oginstagram.com/?hl=invalid", nil)
	request.Header.Set("Cookie", "hl=ko")
	request.Header.Set("Accept-Language", "ja")
	response := httptest.NewRecorder()
	g.serveHome(response, request)
	if !strings.Contains(response.Body.String(), "lang=\"ko\"") || !strings.Contains(response.Body.String(), "href=\"https://oginstagram.com/\"") {
		t.Fatal("cookie or unforced canonical is incorrect")
	}
}

func TestHomeCanonicalRedirectAndHead(t *testing.T) {
	g := testHomeGateway(t)
	request := httptest.NewRequest(http.MethodGet, "https://www.oginstagram.com/?hl=ko&other=secret", nil)
	response := httptest.NewRecorder()
	g.serveHome(response, request)
	if response.Code != 308 || response.Header().Get("Location") != "https://oginstagram.com/?hl=ko" {
		t.Fatalf("incorrect canonical redirect: %d %s", response.Code, response.Header().Get("Location"))
	}
	request = httptest.NewRequest(http.MethodHead, "https://oginstagram.com/", nil)
	response = httptest.NewRecorder()
	g.serveHome(response, request)
	if response.Code != 200 || response.Body.Len() != 0 || response.Header().Get("Content-Length") == "" {
		t.Fatal("HEAD did not preserve page metadata")
	}
}

func TestHomeConfigurationEscaping(t *testing.T) {
	g := testHomeGateway(t)
	g.cfg.Version = "test\"</script><script>alert(1)</script>"
	g.cfg.TurnstileSiteKey = "key\n\"<"
	response := httptest.NewRecorder()
	g.serveHome(response, httptest.NewRequest(http.MethodGet, "https://oginstagram.com/", nil))
	body := response.Body.String()
	data := regexp.MustCompile("<script id=\"app-data\" type=\"application/json\">(.*?)</script>").FindStringSubmatch(body)
	if len(data) != 2 {
		t.Fatal("JSON script was broken")
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(data[1]), &values); err != nil {
		t.Fatal(err)
	}
	if values["version"] != g.cfg.Version || values["key"] != g.cfg.TurnstileSiteKey || strings.Contains(body, "<script>alert") {
		t.Fatal("configuration was not safely encoded")
	}
}

func TestAssetsDenyTemplatesTraversalAndDirectories(t *testing.T) {
	g := testHomeGateway(t)
	if err := os.Mkdir(filepath.Join(g.cfg.AssetsDir, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.cfg.AssetsDir, "assets", "app.js"), []byte("console.log(1)"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(g.cfg.AssetsDir, "assets", "escaped.txt")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/home/en.html", "/home", "/.assetsignore", "/robots.txt", "/favicon.ico", "/sitemap.xml", "/assets/", "/assets", "/assets/../../secret.txt", "/assets/.env", "/assets/escaped.txt"} {
		response := httptest.NewRecorder()
		if !g.serveAsset(response, httptest.NewRequest(http.MethodGet, "https://oginstagram.com"+target, nil)) || response.Code != 404 {
			t.Errorf("private asset %s exposed with %d", target, response.Code)
		}
		if strings.Contains(response.Body.String(), "outside secret") {
			t.Fatal("symlink escaped asset root")
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://oginstagram.com/assets/app.js", nil)
	request.Header.Set("Range", "bytes=0-6")
	response := httptest.NewRecorder()
	if !g.serveAsset(response, request) || response.Code != 206 || response.Body.String() != "console" {
		t.Fatalf("static range not served: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatal("asset caching header missing")
	}
	if g.serveAsset(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://oginstagram.com/p/Example", nil)) {
		t.Fatal("asset handler intercepted embed route")
	}
	if g.serveAsset(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://oginstagram.com/_tiny.sun_", nil)) {
		t.Fatal("asset handler intercepted underscore username")
	}
	if g.serveAsset(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "https://oginstagram.com/.well-known/webfinger", nil)) {
		t.Fatal("asset handler intercepted WebFinger route")
	}
	response = httptest.NewRecorder()
	if !g.serveAsset(response, httptest.NewRequest(http.MethodPost, "https://oginstagram.com/assets/app.js", nil)) || response.Code != 405 || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("asset handler accepted a mutating method")
	}
}
