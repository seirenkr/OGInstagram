package main

import (
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func validProductionConfig() Config {
	return Config{BaseURL: "https://oginstagram.com", AllowedHosts: []string{"oginstagram.com"}, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.30.0.3/32")}, ProxyUser: "user", ProxyPass: "pass", TurnstileSiteKey: "site", TurnstileSecretKey: "secret", OffloadSigningKeys: "keys"}
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
		{"no offload keys", func(c *Config) { c.OffloadSigningKeys = "" }},
		{"trust everyone", func(c *Config) { c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")} }},
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
	cfg.Development, cfg.ProxyUser, cfg.ProxyPass, cfg.OffloadSigningKeys = true, "", "", ""
	if err := cfg.validate(); err != nil {
		t.Fatalf("local development without proxy credentials or offload keys rejected: %v", err)
	}
}

func TestProxyURLRoundTripsCredentials(t *testing.T) {
	u, err := url.Parse(proxyURL("user;x", "pass word", "abc"))
	if err != nil {
		t.Fatal(err)
	}
	if pass, _ := u.User.Password(); u.User.Username() != "user;x__cr.us;sessid.abc" || pass != "pass word" {
		t.Fatalf("proxy credentials = %q", u.User)
	}
}

func TestConfigRejectsMalformedTrustedProxy(t *testing.T) {
	t.Setenv("DEVELOPMENT", "true")
	t.Setenv("TRUSTED_PROXIES", "172.30.0.typo")
	if err := configFromEnv().validate(); err == nil {
		t.Fatal("invalid trusted proxy discarded")
	}
}

func TestAllowedHostsFollowBaseURL(t *testing.T) {
	t.Setenv("BASE_URL", "https://OGInstagram.com")
	if got := configFromEnv().AllowedHosts; !slices.Equal(got, []string{"oginstagram.com", "www.oginstagram.com", "d.oginstagram.com", "g.oginstagram.com"}) {
		t.Fatalf("AllowedHosts = %v", got)
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
