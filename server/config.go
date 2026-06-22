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
	instagramDocID  = "8845758582119845"

	proxyGateEndpoint       = "gate.decodo.com:7000"
	proxyCountry            = "us"
	proxySessionDurationMin = 10
	proxySessionCount       = 10

	defaultCacheTTLSeconds  = 3600
	negativeCacheTTL        = 60 * time.Second
	defaultProxyHourlyLimit = 180

	fetchRaceCount     = 2
	fetchRaceExplorers = 1
	ewmaAlpha          = 0.3
	fetchTimeout       = 4500 * time.Millisecond

	fetchHedgeDelay = 1500 * time.Millisecond
	fetchHedgeCount = 1

	rotateCooldown  = 3 * time.Second
	maxCacheEntries = 500

	maxResponseBytes = 1 << 20

	maxInlineVideoBytes = 200 << 20

	edgeCacheSeconds     = 3600
	homeEdgeCacheSeconds = 120
	homeBrowserCacheSecs = 60
	iconCacheSeconds     = 86400

	errorEmbedCacheSecond = 600

	serviceName          = "oginstagram"
	rateLimitTitle       = "Rate Limit Exceeded"
	rateLimitDescription = "Instagram fetching is temporarily rate limited. Please try again later."

	instagramAppUA = "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)"

	snowcodeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789{}[]\":,.-_"
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
		Port:            port,
		Version:         version,
		DecodoUser:      strings.TrimSpace(os.Getenv("DECODO_USERNAME")),
		DecodoPass:      strings.TrimSpace(os.Getenv("DECODO_PASSWORD")),
		BrandName:       brandName,
		BrandColor:      brandColor,
		SupportURL:      supportURL,
		GitHubURL:       githubURL,
		BaseURL:         strings.TrimSpace(os.Getenv("BASE_URL")),
		CacheTTLSeconds: defaultCacheTTLSeconds,
		HourlyLimit:     defaultProxyHourlyLimit,
		AssetsDir:       assets,
		LocalPreview:    preview,
	}
}

func decodoProxyURL(user, pass, sessionID string) string {
	username := "user-" + user + "-country-" + proxyCountry +
		"-session-" + sessionID + "-sessionduration-" + strconv.Itoa(proxySessionDurationMin)
	return "http://" + url.QueryEscape(username) + ":" + url.QueryEscape(pass) + "@" + proxyGateEndpoint
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
