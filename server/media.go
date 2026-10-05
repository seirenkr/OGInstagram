package main

import (
	"net/url"
	"strings"
)

// trustedMediaURL accepts only HTTPS Instagram CDN URLs as redirect targets.
func trustedMediaURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	return host == "cdninstagram.com" || strings.HasSuffix(host, ".cdninstagram.com") ||
		host == "fbcdn.net" || strings.HasSuffix(host, ".fbcdn.net")
}
