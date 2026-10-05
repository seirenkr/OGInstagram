package main

import (
	"net/netip"
	"time"
)

type Attachment struct {
	ID        string
	Kind      string
	URL       string
	Thumbnail string
	Width     int
	Height    int
}

type Post struct {
	Shortcode   string
	Username    string
	OwnerID     string
	FullName    string
	ProfilePic  string
	IsVerified  bool
	Caption     string
	StatsLine   string
	Attachments []Attachment
	CreatedAt   time.Time
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
	BaseURL                string
	OffloadSigningKeys     string
	DiscordBrandEmojiID    string
	DiscordVerifiedEmojiID string
	WorkerHubSignKey       string
	WorkerHubSignTS        string
	DataDir                string
	AssetsDir              string
	BudgetStartDate        string
	AllowedHosts           []string
	TrustedProxies         []netip.Prefix
	TurnstileSiteKey       string
	TurnstileSecretKey     string
	AdminPurgeToken        string
	Development            bool
	Store                  *localStore
}
