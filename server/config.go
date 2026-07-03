package main

import (
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	instagramOrigin = "https://www.instagram.com"
	instagramAppID  = "936619743392459"

	instagramWebLoggedOutDocID = "27128499623469141"

	snowcodeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789{}[]\":,.-_"

	proxyGateEndpoint = "gw.dataimpulse.com:823"
	proxyCountry      = "us"
	proxySessionCount = 10

	defaultProxyHourlyLimit = 1000
	// defaultGlobalHourlyLimit caps total proxy requests/hour across all sessions
	// for predictable (residential-bandwidth) cost. 0 = unlimited.
	// Sized for ~100GB/month: 137MB/h at an assumed ~55KB wire per GraphQL
	// round trip ≈ 2500 req/h; tune against the provider bandwidth dashboard.
	defaultGlobalHourlyLimit = 2500

	transientErrorCacheSeconds = 300
	permanentErrorCacheSeconds = 3600

	fetchRaceCount = 1
	ewmaAlpha      = 0.3
	fetchTimeout   = 4500 * time.Millisecond

	fetchHedgeDelay = 1500 * time.Millisecond
	fetchHedgeCount = 1

	// headProbeTimeout caps the CDN HEAD size probe so a slow CDN can't hold
	// the whole post response; on timeout the video counts as not oversized.
	headProbeTimeout = 1500 * time.Millisecond

	rotateCooldown  = 3 * time.Second
	maxCacheEntries = 5000

	maxResponseBytes = 1 << 20

	maxInlineVideoBytes = 200 << 20

	edgeCacheSeconds        = 3600
	homeEdgeCacheSeconds    = 120
	homeBrowserCacheSeconds = 60
	iconCacheSeconds        = 86400

	serviceName = "oginstagram"

	budgetTitle       = "Hourly limit reached"
	budgetDescription = "This service has reached its hourly request limit. Please try again later."

	instagramAppUA = "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)"
	instagramWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.6261.112 Safari/537.36"
	// embedUA is intentionally non-browser: IG server-renders the embed payload
	// only for crawler-like UAs (a modern Chrome UA gets an empty JS shell).
	embedUA         = "facebookexternalhit/1.1"
	instagramAsbdID = "129477"
)

func configFromEnv() Config {
	port := 8080
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PORT"))); err == nil && v > 0 {
		port = v
	}
	assets := envString("ASSETS_DIR", "/app/assets")
	preview := envBool("LOCAL_PREVIEW")
	brandName := strings.TrimSpace(os.Getenv("BRAND_NAME"))
	brandColor := strings.TrimSpace(os.Getenv("BRAND_COLOR"))
	supportURL := strings.TrimSpace(os.Getenv("SUPPORT_URL"))
	githubURL := envString("GITHUB_URL", "https://github.com/LilasKR/OGInstagram")
	version := envString("OG_VERSION", "dev")
	if preview {
		if brandName == "" {
			brandName = "OGInstagram"
		}
		if brandColor == "" {
			brandColor = "#f48120"
		}
		if supportURL == "" {
			supportURL = "https://ko-fi.com/voyage1"
		}
		if version == "dev" {
			version = "2.5.2-local"
		}
	}
	return Config{
		Port:              port,
		Version:           version,
		ProxyUser:         strings.TrimSpace(os.Getenv("PROXY_USERNAME")),
		ProxyPass:         strings.TrimSpace(os.Getenv("PROXY_PASSWORD")),
		BrandName:         brandName,
		BrandColor:        brandColor,
		SupportURL:        supportURL,
		GitHubURL:         githubURL,
		BaseURL:           strings.TrimSpace(os.Getenv("BASE_URL")),
		GlobalHourlyLimit: envInt("PROXY_HOURLY_LIMIT", defaultGlobalHourlyLimit),
		AssetsDir:         assets,
	}
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return fallback
}

// proxyURL builds a DataImpulse gateway URL: login__cr.<country>;sessid.<id>
// pins a residential IP to the session id (sticky ~30 min).
func proxyURL(user, pass, sessionID string) string {
	username := user + "__cr." + proxyCountry + ";sessid." + sessionID
	return "http://" + url.QueryEscape(username) + ":" + url.QueryEscape(pass) + "@" + proxyGateEndpoint
}

func newLSD() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 23+rand.IntN(5))
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

func newSessionID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
