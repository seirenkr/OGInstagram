package main

import "time"

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
	Caption    string
	Media      Attachment
	CreatedAt  time.Time
}

type Config struct {
	Port               int
	Version            string
	ProxyUser          string
	ProxyPass          string
	BaseURL            string
	CacheURL           string
	BudgetURL          string
	OffloadSigningKeys string
}
