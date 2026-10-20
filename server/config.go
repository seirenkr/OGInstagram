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
	instagramOrigin        = "https://www.instagram.com"
	instagramAppID         = "936619743392459"
	instagramGraphQLOrigin = "https://i.instagram.com"

	instagramWebLoggedOutDocID = "27128499623469141"

	proxyGateEndpoint = "gw.dataimpulse.com:823"
	proxyCountry      = "us"
	proxySessionCount = 10

	transientErrorCacheSeconds = 60
	permanentErrorCacheSeconds = 3600
	// Cards link only to signed offload URLs, which outlive this by days.
	cardEdgeTTL = 3600

	requestTimeout = 5 * time.Second

	// How fast a session's Peak-EWMA latency fades (tower's decay; Linkerd uses 10 s).
	sessionRTTDecay = 10 * time.Second

	// IPv6 gets this long (at most half the time left) before DC.
	ipv6AttemptTimeout     = time.Second
	ipv6PageAttemptTimeout = 1500 * time.Millisecond
	dcAttemptTimeout       = 2500 * time.Millisecond
	dcRecheckTimeout       = 2 * time.Second // cold DC exits are slow
	ipv6Strikes            = 3               // failures among the last ipv6Window attempts
	ipv6Window             = 10
	ipv6PauseMin           = 2 * time.Minute
	ipv6PauseMax           = time.Hour
	// A DC exit that runs this long into a deadline is treated as hung.
	proxyHungAfter = 3 * time.Second

	postHedgeDelay           = 800 * time.Millisecond
	externalHelperHedgeDelay = 2 * time.Second
	storyHelperHedgeDelay    = 2 * time.Second
	// A profile card waits this long (from the start) for thumbnails.
	profilePostsWait = 1500 * time.Millisecond
	// A late external helper answer still fills the cache.
	helperDetachedTimeout = 25 * time.Second
	ogPageTimeout         = 2500 * time.Millisecond
	ogPageBytes           = 32 << 10
	partialPostTTL        = 10 * time.Minute // a partial post or gate notice stands in this long
	helperBackgroundSlots = 8
	// The content-ruling check is a small JSON GET; past this it is no signal.
	rulingTimeout = 2 * time.Second
	// Past this a profile picture copy gives way to the CDN redirect.
	avatarTimeout = 2500 * time.Millisecond

	rotateCooldown        = 3 * time.Second
	helperCaptchaCooldown = time.Minute

	// Unverified bots get this many requests per IP per hour instead of a
	// managed challenge; Cloudflare marks them with botClassHeader: limited.
	botHourlyLimit = 50
	botClassHeader = "X-OG-Bot-Class"
	// Keep origin work bounded on a single small instance.
	maxConcurrentFetches = 48

	localPostCacheBytes    = 64 << 20
	localProfileCacheBytes = 4 << 20
	localStoryCacheBytes   = 4 << 20
	localOGPostCacheBytes  = 4 << 20

	maxResponseBytes = 1 << 20
	maxAvatarBytes   = 512 << 10 // profile pictures are 5-30 KiB

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 120 * time.Second // > nginx upstream keepalive_timeout (60s default), so nginx closes idle conns first
	shutdownTimeout   = 30 * time.Second
	maxHeaderBytes    = 32 << 10

	serviceName = "oginstagram"

	brandName  = "OGInstagram"
	brandColor = "#ff0069"

	defaultAvatarPath = "/default-avatar.jpg"

	// Public IDs of the emojis registered in the project's Discord server
	// (docs/discord-emojis/README.md).
	defaultDiscordBrandEmojiID    = "1556597080229810266"
	defaultDiscordVerifiedEmojiID = "1556598045670514798"

	instagramAppUA = "Instagram 273.0.0.16.70 (iPhone15,2; iOS 17_5_1; en_US; en-US; scale=3.00; 1290x2796; 470085518)"
	embedUA        = "facebookexternalhit/1.1"
)

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

// egressPercent is the share of Instagram requests that try IPv6 first (default 100).
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
		// Keep invalid entries so validation rejects a misspelled trust boundary.
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

func proxyURL(user, pass, sessionID string) string {
	username := user + "__cr." + proxyCountry + ";sessid." + sessionID
	return (&url.URL{Scheme: "http", User: url.UserPassword(username, pass), Host: proxyGateEndpoint}).String()
}

func newSessionID() string {
	return strings.ToLower(rand.Text()[:8])
}
