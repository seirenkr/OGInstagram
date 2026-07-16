package main

import (
	"cmp"
	"crypto/rand"
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

	proxyGateEndpoint = "gw.dataimpulse.com:823"
	proxyCountry      = "us"
	proxySessionCount = 10

	defaultProxyHourlyLimit = 1000

	transientErrorCacheSeconds = 300
	permanentErrorCacheSeconds = 3600

	ewmaAlpha    = 0.3
	fetchTimeout = 4500 * time.Millisecond

	fetchHedgeDelay = 1500 * time.Millisecond

	headProbeTimeout = 1500 * time.Millisecond

	rotateCooldown       = 3 * time.Second
	maxCacheEntries      = 5000
	maxConcurrentFetches = 6

	maxResponseBytes = 1 << 20

	maxInlineVideoBytes = 200 << 20

	edgeCacheSeconds = 86400
	serviceName      = "oginstagram"

	brandName  = "OGInstagram"
	brandColor = "#ff0069"
	supportURL = "https://ko-fi.com/seirenkr"

	defaultAvatarPath = "/default-avatar.jpg"

	budgetTitle       = "Hourly limit reached"
	budgetDescription = "This service has reached its hourly request limit. Please try again later."

	instagramAppUA = "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)"
	instagramWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.6261.112 Safari/537.36"

	embedUA         = "facebookexternalhit/1.1"
	instagramAsbdID = "129477"
)

func configFromEnv() Config {
	port := 8080
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PORT"))); err == nil && v > 0 {
		port = v
	}
	return Config{
		Port:          port,
		Version:       cmp.Or(strings.TrimSpace(os.Getenv("OG_VERSION")), "dev"),
		ProxyUser:     strings.TrimSpace(os.Getenv("PROXY_USERNAME")),
		ProxyPass:     strings.TrimSpace(os.Getenv("PROXY_PASSWORD")),
		BaseURL:       strings.TrimSpace(os.Getenv("BASE_URL")),
		ModelCacheURL: strings.TrimSpace(os.Getenv("MODEL_CACHE_URL")),
		BudgetURL:     strings.TrimSpace(os.Getenv("BUDGET_URL")),
	}
}

func proxyURL(user, pass, sessionID string) string {
	username := user + "__cr." + proxyCountry + ";sessid." + sessionID
	return "http://" + url.QueryEscape(username) + ":" + url.QueryEscape(pass) + "@" + proxyGateEndpoint
}

func newSessionID() string {
	return strings.ToLower(rand.Text()[:8])
}
