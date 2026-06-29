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
	Port            int
	Version         string
	DecodoUser      string
	DecodoPass      string
	BrandName       string
	BrandColor      string
	SupportURL      string
	GitHubURL       string
	BaseURL         string
	CacheTTLSeconds int
	HourlyLimit     int
	AssetsDir       string
	LocalPreview    bool
}

type RequestQuery struct {
	ImgIndex    int
	ImgIndexSet bool
	Index       int
	IndexSet    bool
	Order       int
	OrderSet    bool
	Gallery     bool
}

type AppError struct {
	Status  int
	Message string
	Reason  string

	Ephemeral bool
}

func newAppError(status int, message string) *AppError {
	return &AppError{Status: status, Message: message}
}

func igErr(status int, reason, message string) *AppError {
	return &AppError{Status: status, Message: message, Reason: reason}
}

func ephemeralErr(status int, reason, message string) *AppError {
	return &AppError{Status: status, Message: message, Reason: reason, Ephemeral: true}
}
