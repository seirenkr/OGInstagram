package main

import "time"

type Attachment struct {
	ID        string
	Kind      string
	URL       string
	Thumbnail string
	Width     int
	Height    int

	OversizedInline bool
}

type Post struct {
	Shortcode   string
	Username    string
	OwnerID     string
	FullName    string
	ProfilePic  string
	Caption     string
	StatsLine   string
	Attachments []Attachment
	CreatedAt   time.Time
}

type Config struct {
	Port              int
	Version           string
	ProxyUser         string
	ProxyPass         string
	BrandName         string
	BrandColor        string
	SupportURL        string
	GitHubURL         string
	BaseURL           string
	GlobalHourlyLimit int
	AssetsDir         string
}

type AppError struct {
	Status  int
	Message string
	Reason  string

	Ephemeral bool

	// Error-card overrides from the oembed fallback: Instagram's own error
	// text, shown instead of the generic per-reason card.
	CardReason, CardTitle, CardDesc string
}

func igErr(status int, reason, message string) *AppError {
	return &AppError{Status: status, Message: message, Reason: reason}
}

func ephemeralErr(status int, reason, message string) *AppError {
	return &AppError{Status: status, Message: message, Reason: reason, Ephemeral: true}
}
