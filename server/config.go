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

	defaultProxyHourlyLimit       = 1000
	proxyByteLeaseSize      int64 = 1 << 20

	// Intercepted container outbound is a Worker subrequest, so it queues behind
	// the Worker's 6 simultaneous-connection limit; 300ms was under the observed
	// p50 and failed closed, blocking the proxy source.
	budgetRequestTimeout    = time.Second
	budgetBackendRetryDelay = time.Second

	transientErrorCacheSeconds = 300
	permanentErrorCacheSeconds = 3600

	requestTimeout = 4 * time.Second

	ewmaAlpha = 0.3

	postHedgeDelay           = time.Second
	profileHedgeDelay        = time.Second
	externalHelperHedgeDelay = 2 * time.Second
	oembedHedgeDelay         = 2500 * time.Millisecond

	rotateCooldown = 3 * time.Second
	// Global in-flight fetch cap, sized against the container's memory, not any
	// connection limit: lite is 256 MiB, ~12 MiB of it runtime and 24 MiB L1.
	maxConcurrentFetches = 24

	localPostCacheBytes    = 16 << 20
	localProfileCacheBytes = 4 << 20
	localStoryCacheBytes   = 4 << 20

	maxResponseBytes = 1 << 20

	readHeaderTimeout = 5 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 30 * time.Second
	maxHeaderBytes    = 32 << 10

	edgeCacheSeconds                = 86400
	edgeStaleWhileRevalidateSeconds = 3600
	edgeStaleIfErrorSeconds         = 86400
	serviceName                     = "oginstagram"

	brandName  = "OGInstagram"
	brandColor = "#ff0069"

	defaultAvatarPath = "/default-avatar.jpg"

	instagramAppUA = "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)"
	instagramWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.6261.112 Safari/537.36"

	embedUA         = "facebookexternalhit/1.1"
	instagramAsbdID = "129477"
)

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func configFromEnv() Config {
	port := 8080
	if v, err := strconv.Atoi(env("PORT")); err == nil && v > 0 {
		port = v
	}
	return Config{
		Port:               port,
		Version:            cmp.Or(env("OG_VERSION"), "dev"),
		ProxyUser:          env("PROXY_USERNAME"),
		ProxyPass:          env("PROXY_PASSWORD"),
		BaseURL:            env("BASE_URL"),
		CacheURL:           env("CACHE_URL"),
		BudgetURL:          env("BUDGET_URL"),
		OffloadSigningKeys: env("OFFLOAD_SIGNING_KEYS"),
	}
}

func proxyURL(user, pass, sessionID string) string {
	username := user + "__cr." + proxyCountry + ";sessid." + sessionID
	return "http://" + url.QueryEscape(username) + ":" + url.QueryEscape(pass) + "@" + proxyGateEndpoint
}

func newSessionID() string {
	return strings.ToLower(rand.Text()[:8])
}
