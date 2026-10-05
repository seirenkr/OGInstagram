package main

import (
	"cmp"
	"crypto/rand"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"slices"
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

	proxyByteLeaseSize int64 = 1 << 20

	// Bound durable budget grants; errors stop proxy traffic until retry.
	budgetRequestTimeout    = time.Second
	budgetBackendRetryDelay = time.Second

	transientErrorCacheSeconds = 60
	permanentErrorCacheSeconds = 3600

	requestTimeout = 4 * time.Second

	ewmaAlpha = 0.3

	postHedgeDelay           = time.Second
	profileHedgeDelay        = time.Second
	externalHelperHedgeDelay = 2 * time.Second
	oembedHedgeDelay         = 2500 * time.Millisecond
	// A ready oEmbed card ends a still-running staged race this early, so the
	// degraded card is served instead of a timeout.
	oembedFallbackAt = requestTimeout - 500*time.Millisecond

	rotateCooldown = 3 * time.Second
	// Keep origin work bounded on the single small Vultr instance.
	maxConcurrentFetches = 48

	localPostCacheBytes    = 16 << 20
	localProfileCacheBytes = 4 << 20
	localStoryCacheBytes   = 4 << 20

	maxResponseBytes = 1 << 20

	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 10 * time.Second
	idleTimeout       = 120 * time.Second // > cloudflared keepAliveTimeout (90s), so the proxy closes idle conns first
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
	instagramWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.6261.112 Safari/537.36"

	embedUA         = "facebookexternalhit/1.1"
	instagramAsbdID = "129477"
)

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func configFromEnv() Config {
	port := 8080
	if raw := env("PORT"); raw != "" {
		port, _ = strconv.Atoi(raw)
	}
	baseURL := cmp.Or(env("BASE_URL"), "https://oginstagram.com")
	hosts := envList("ALLOWED_HOSTS")
	if len(hosts) == 0 {
		if base, err := url.Parse(baseURL); err == nil && base.Hostname() != "" {
			h := base.Hostname()
			hosts = []string{h, "www." + h, "d." + h, "www.d." + h, "g." + h, "www.g." + h}
		}
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
		Port:               port,
		Version:            cmp.Or(env("OG_VERSION"), "dev"),
		ProxyUser:          env("PROXY_USERNAME"),
		ProxyPass:          env("PROXY_PASSWORD"),
		BaseURL:            strings.TrimRight(baseURL, "/"),
		OffloadSigningKeys: env("OFFLOAD_SIGNING_KEYS"),
		DataDir:            cmp.Or(env("DATA_DIR"), "./data"),
		AssetsDir:          cmp.Or(env("ASSETS_DIR"), "../web/dist"),
		// The ledger only moves the start forward (MAX), so today is a safe default.
		BudgetStartDate:        cmp.Or(env("PROXY_BUDGET_START_DATE"), time.Now().UTC().Format(time.DateOnly)),
		AllowedHosts:           hosts,
		TrustedProxies:         proxies,
		TurnstileSiteKey:       env("TURNSTILE_SITE_KEY"),
		TurnstileSecretKey:     env("TURNSTILE_SECRET_KEY"),
		AdminPurgeToken:        env("ADMIN_PURGE_TOKEN"),
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
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535")
	}
	if len(cfg.AllowedHosts) == 0 {
		return fmt.Errorf("ALLOWED_HOSTS is required")
	}
	for _, host := range cfg.AllowedHosts {
		if strings.ContainsAny(host, "/:@?# ") || host == "" || strings.IndexFunc(host, func(r rune) bool { return r <= 32 || r == 92 }) >= 0 {
			return fmt.Errorf("ALLOWED_HOSTS must contain hostnames without ports")
		}
	}
	for _, proxy := range cfg.TrustedProxies {
		if !proxy.IsValid() || proxy.Bits() == 0 {
			return fmt.Errorf("TRUSTED_PROXIES must contain explicit IP addresses or bounded CIDRs")
		}
	}
	if _, err := time.Parse("2006-01-02", cfg.BudgetStartDate); err != nil {
		return fmt.Errorf("PROXY_BUDGET_START_DATE is required in YYYY-MM-DD UTC format")
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
	if !cfg.Development && (base.Port() != "" || !slices.Contains(cfg.AllowedHosts, strings.ToLower(base.Hostname()))) {
		return fmt.Errorf("production BASE_URL host must be listed in ALLOWED_HOSTS, without a port")
	}
	return nil
}

func proxyURL(user, pass, sessionID string) string {
	username := user + "__cr." + proxyCountry + ";sessid." + sessionID
	return "http://" + url.QueryEscape(username) + ":" + url.QueryEscape(pass) + "@" + proxyGateEndpoint
}

func newSessionID() string {
	return strings.ToLower(rand.Text()[:8])
}
