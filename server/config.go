package main

import (
	"cmp"
	"crypto/rand"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	instagramOrigin = "https://www.instagram.com"
	instagramAppID  = "936619743392459"

	instagramWebLoggedOutDocID = "28891146940585796"
	// Elias gamma bitmap for 200,000 loaded resources so GraphQL omits extensions.sr_payload.
	haveAllResources = "w00MR0"

	proxyGateEndpoint = "gw.dataimpulse.com:823"
	proxyCountry      = "us"
	proxySessionCount = 10

	transientErrorCacheSeconds = 60
	permanentErrorCacheSeconds = 3600
	cardEdgeTTL                = 3600

	requestTimeout = 5 * time.Second

	sessionRTTDecay = 10 * time.Second

	ipv6AttemptTimeout     = 2 * time.Second
	ipv6PageAttemptTimeout = 1500 * time.Millisecond
	ipv6MinBudget          = 150 * time.Millisecond
	dcAttemptTimeout       = 2500 * time.Millisecond
	dcRecheckTimeout       = 2 * time.Second
	ipv6Strikes            = 3
	ipv6Retries            = 2
	egressPoolSize         = 64               // matches tools/ipv6-pool.sh
	egressAddrCooldown     = 30 * time.Minute // Instagram keeps per-IPv6 throttle verdicts ~30m
	ipv6Window             = 10
	ipv6PauseMin           = 2 * time.Minute
	ipv6PauseMax           = time.Hour
	proxyHungAfter         = 3 * time.Second

	postHedgeDelay           = 800 * time.Millisecond
	externalHelperHedgeDelay = 2 * time.Second
	storyHelperHedgeDelay    = 2 * time.Second
	profilePostsWait         = 1500 * time.Millisecond
	helperDetachedTimeout    = 25 * time.Second
	ogPageTimeout            = 2500 * time.Millisecond
	ogPageBytes              = 32 << 10
	partialPostTTL           = 10 * time.Minute
	helperBackgroundSlots    = 8
	rulingTimeout            = 2 * time.Second
	avatarTimeout            = 2500 * time.Millisecond

	rotateCooldown        = 3 * time.Second
	helperCaptchaCooldown = time.Minute

	// Cloudflare Free rate-limiting only counts over 10s; a Transform Rule sets botClassHeader.
	botHourlyLimit       = 50
	botClassHeader       = "X-OG-Bot-Class"
	maxConcurrentFetches = 48

	localPostCacheBytes    = 64 << 20
	localProfileCacheBytes = 4 << 20
	localStoryCacheBytes   = 4 << 20
	localShareCacheBytes   = 4 << 20
	localOGPostCacheBytes  = 4 << 20

	maxResponseBytes = 1 << 20
	maxAvatarBytes   = 512 << 10

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 120 * time.Second // > nginx keepalive_timeout (60s), so nginx closes idle conns first
	shutdownTimeout   = 30 * time.Second
	maxHeaderBytes    = 32 << 10

	serviceName = "oginstagram"

	brandName  = "OGInstagram"
	brandColor = "#ff0069"

	defaultAvatarPath = "/default-avatar.jpg"

	defaultDiscordBrandEmojiID    = "1556597080229810266"
	defaultDiscordVerifiedEmojiID = "1556598045670514798"

	instagramAppUA = "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)"
	embedUA        = "facebookexternalhit/1.1"
)

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func egressPercent(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 100 {
		return 100
	}
	return n
}

func configFromEnv() Config {
	baseURL := cmp.Or(env("BASE_URL"), "https://oginstagram.com")
	var hosts []string
	if base, err := url.Parse(baseURL); err == nil && base.Hostname() != "" {
		h := strings.ToLower(base.Hostname())
		hosts = []string{h, "www." + h, "d." + h, "g." + h}
	}
	var proxies []netip.Prefix
	for _, raw := range envList("TRUSTED_PROXIES") {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			if addr, addrErr := netip.ParseAddr(raw); addrErr == nil {
				p = netip.PrefixFrom(addr, addr.BitLen())
			}
		}
		proxies = append(proxies, p)
	}
	return Config{
		Version:                cmp.Or(env("OG_VERSION"), "dev"),
		ProxyUser:              env("PROXY_USERNAME"),
		ProxyPass:              env("PROXY_PASSWORD"),
		EgressSocket:           env("EGRESS_SOCKET"),
		EgressPercent:          egressPercent(env("EGRESS_PERCENT")),
		BaseURL:                strings.TrimRight(baseURL, "/"),
		OffloadSigningKeys:     env("OFFLOAD_SIGNING_KEYS"),
		DataDir:                cmp.Or(env("DATA_DIR"), "./data"),
		AssetsDir:              cmp.Or(env("ASSETS_DIR"), "../web/dist"),
		AllowedHosts:           hosts,
		TrustedProxies:         proxies,
		TurnstileSiteKey:       env("TURNSTILE_SITE_KEY"),
		TurnstileSecretKey:     env("TURNSTILE_SECRET_KEY"),
		DiscordBrandEmojiID:    cmp.Or(env("DISCORD_BRAND_EMOJI_ID"), defaultDiscordBrandEmojiID),
		DiscordVerifiedEmojiID: cmp.Or(env("DISCORD_VERIFIED_EMOJI_ID"), defaultDiscordVerifiedEmojiID),
		Development:            env("DEVELOPMENT") == "true",
	}
}

func envList(key string) []string {
	var values []string
	for _, value := range strings.Split(env(key), ",") {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func (cfg Config) validate() error {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") ||
		(base.Scheme != "https" && !(cfg.Development && base.Scheme == "http")) {
		return fmt.Errorf("BASE_URL must be an HTTPS origin (HTTP is allowed in development)")
	}
	for _, proxy := range cfg.TrustedProxies {
		if !proxy.IsValid() || proxy.Bits() == 0 {
			return fmt.Errorf("TRUSTED_PROXIES must contain explicit IP addresses or bounded CIDRs")
		}
	}
	if (cfg.ProxyUser == "") != (cfg.ProxyPass == "") {
		return fmt.Errorf("PROXY_USERNAME and PROXY_PASSWORD must be set together")
	}
	for key, id := range map[string]string{
		"DISCORD_BRAND_EMOJI_ID":    cfg.DiscordBrandEmojiID,
		"DISCORD_VERIFIED_EMOJI_ID": cfg.DiscordVerifiedEmojiID,
	} {
		if id != "" && !validDiscordEmojiID(id) {
			return fmt.Errorf("%s must be a Discord emoji snowflake ID", key)
		}
	}
	if !cfg.Development && cfg.ProxyUser == "" {
		return fmt.Errorf("production requires PROXY_USERNAME and PROXY_PASSWORD")
	}
	if !cfg.Development && (len(cfg.TrustedProxies) == 0 || cfg.TurnstileSiteKey == "" || cfg.TurnstileSecretKey == "") {
		return fmt.Errorf("production requires TRUSTED_PROXIES and Turnstile keys")
	}
	if !cfg.Development && cfg.OffloadSigningKeys == "" {
		return fmt.Errorf("production requires OFFLOAD_SIGNING_KEYS")
	}
	if !cfg.Development && base.Port() != "" {
		return fmt.Errorf("production BASE_URL must not carry a port")
	}
	return nil
}

func proxyURL(user, pass, sessionID string) *url.URL {
	username := user + "__cr." + proxyCountry + ";sessid." + sessionID
	return &url.URL{Scheme: "http", User: url.UserPassword(username, pass), Host: proxyGateEndpoint}
}

func newSessionID() string {
	return strings.ToLower(rand.Text()[:8])
}
