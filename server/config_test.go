package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func validProductionConfig() Config {
	return Config{Port: 8080, BaseURL: "https://oginstagram.com", BudgetStartDate: "2026-10-06", AllowedHosts: []string{"oginstagram.com"}, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.30.0.3/32")}, ProxyUser: "user", ProxyPass: "pass", TurnstileSiteKey: "site", TurnstileSecretKey: "secret", AdminPurgeToken: "token"}
}

func TestProductionConfigurationRejectsIncompleteTrustAndCredentials(t *testing.T) {
	if err := validProductionConfig().validate(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"no proxy", func(c *Config) { c.ProxyUser, c.ProxyPass = "", "" }},
		{"half proxy", func(c *Config) { c.ProxyPass = "" }},
		{"no trusted peer", func(c *Config) { c.TrustedProxies = nil }},
		{"trust everyone", func(c *Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")} }},
		{"host with port", func(c *Config) { c.AllowedHosts = []string{"oginstagram.com:80"} }},
		{"malformed host", func(c *Config) { c.AllowedHosts = []string{"oginstagram.com\t"} }},
		{"no quota start", func(c *Config) { c.BudgetStartDate = "" }},
		{"base host not allowed", func(c *Config) { c.AllowedHosts = []string{"www.oginstagram.com"} }},
		{"base URL with port", func(c *Config) { c.BaseURL = "https://oginstagram.com:8443" }},
		{"HTTP production", func(c *Config) { c.BaseURL = "http://oginstagram.com" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validProductionConfig()
			tt.mutate(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("invalid production configuration accepted")
			}
		})
	}
	cfg := validProductionConfig()
	cfg.Development, cfg.ProxyUser, cfg.ProxyPass = true, "", ""
	if err := cfg.validate(); err != nil {
		t.Fatalf("local development without proxy credentials rejected: %v", err)
	}
}

func TestConfigRejectsMalformedPortAndTrustedProxy(t *testing.T) {
	t.Setenv("PORT", "not-a-port")
	t.Setenv("PROXY_BUDGET_START_DATE", "2026-10-06")
	t.Setenv("DEVELOPMENT", "true")
	if err := configFromEnv().validate(); err == nil {
		t.Fatal("invalid port accepted")
	}
	t.Setenv("PORT", "8080")
	t.Setenv("TRUSTED_PROXIES", "172.30.0.typo")
	if err := configFromEnv().validate(); err == nil {
		t.Fatal("invalid trusted proxy discarded")
	}
}

func TestDiscordEmojiConfiguration(t *testing.T) {
	for _, key := range []string{"DISCORD_BRAND_EMOJI_ID", "DISCORD_VERIFIED_EMOJI_ID"} {
		t.Run(key, func(t *testing.T) {
			for _, value := range []string{"", "123456789012345678"} {
				t.Setenv(key, value)
				parsed := configFromEnv()
				got := parsed.DiscordBrandEmojiID
				if key == "DISCORD_VERIFIED_EMOJI_ID" {
					got = parsed.DiscordVerifiedEmojiID
				}
				want := value
				if want == "" {
					want = map[string]string{"DISCORD_BRAND_EMOJI_ID": defaultDiscordBrandEmojiID, "DISCORD_VERIFIED_EMOJI_ID": defaultDiscordVerifiedEmojiID}[key]
				}
				if got != want {
					t.Fatalf("%s = %q; want %q", key, got, want)
				}
				cfg := validProductionConfig()
				cfg.DiscordBrandEmojiID, cfg.DiscordVerifiedEmojiID = parsed.DiscordBrandEmojiID, parsed.DiscordVerifiedEmojiID
				if err := cfg.validate(); err != nil {
					t.Fatalf("optional emoji ID %q was rejected: %v", value, err)
				}
			}
			for _, value := range []string{"0", "00000000000000000", "1234", "18446744073709551616", "+123456789012345678", "<:logo:123456789012345678>", "https://example.com/logo.png", "12345678901234567> @everyone"} {
				t.Setenv(key, value)
				parsed := configFromEnv()
				cfg := validProductionConfig()
				cfg.DiscordBrandEmojiID, cfg.DiscordVerifiedEmojiID = parsed.DiscordBrandEmojiID, parsed.DiscordVerifiedEmojiID
				if err := cfg.validate(); err == nil {
					t.Errorf("malformed emoji ID %q was accepted", value)
				}
			}
		})
	}
}

func TestLoadHomeTemplatesRequiresEveryLocale(t *testing.T) {
	g := testHomeGateway(t)
	if pages, err := loadHomeTemplates(g.cfg.AssetsDir); err != nil || len(pages) != len(homeLocales) {
		t.Fatal(len(pages), err)
	}
	if err := os.Remove(filepath.Join(g.cfg.AssetsDir, "home", "ko.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHomeTemplates(g.cfg.AssetsDir); err == nil {
		t.Fatal("incomplete frontend build accepted")
	}
}

func TestOptionalSettingsHaveSafeDefaults(t *testing.T) {
	t.Setenv("PROXY_BUDGET_START_DATE", "")
	if got, want := configFromEnv().BudgetStartDate, time.Now().UTC().Format(time.DateOnly); got != want {
		t.Errorf("default budget start = %q, want today %q", got, want)
	}
	cfg := validProductionConfig()
	cfg.AdminPurgeToken = ""
	if err := cfg.validate(); err != nil {
		t.Errorf("ADMIN_PURGE_TOKEN must be optional (purge disabled): %v", err)
	}

	dir := t.TempDir()
	first, err := loadOrCreateOffloadKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateOffloadKeys(dir)
	if err != nil || second != first {
		t.Fatalf("generated keyring must persist across restarts: %q vs %q (%v)", first, second, err)
	}
	if _, err := parseOffloadSigner(first); err != nil {
		t.Fatalf("generated keyring is invalid: %v", err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "offload-signing-keys.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("keyring file mode = %v, want 0600", info.Mode().Perm())
	}
}
