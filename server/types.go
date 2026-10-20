package main

import (
	"net/netip"
	"time"
)

type Attachment struct {
	Kind      string
	URL       string
	Thumbnail string
	Width     int
	Height    int
}

type Post struct {
	Shortcode   string
	Username    string
	FullName    string
	ProfilePic  string
	IsVerified  bool
	Caption     string
	StatsLine   string
	Attachments []Attachment
	CreatedAt   time.Time
	// Partial: built from the Open Graph tags Instagram serves crawlers (one
	// cover image). A full source replaces it, so it is never persisted.
	Partial bool `json:",omitempty"`
}

type Story struct {
	ID         string
	Username   string
	FullName   string
	ProfilePic string
	IsVerified bool
	Caption    string
	Media      Attachment
	CreatedAt  time.Time
}

type Config struct {
	Port                   int
	Version                string
	ProxyUser              string
	ProxyPass              string
	EgressSocket           string
	EgressPercent          int
	BaseURL                string
	OffloadSigningKeys     string
	DiscordBrandEmojiID    string
	DiscordVerifiedEmojiID string
	DataDir                string
	AssetsDir              string
	AllowedHosts           []string
	TrustedProxies         []netip.Prefix
	TurnstileSiteKey       string
	TurnstileSecretKey     string
	Development            bool
	Store                  *localStore
}
