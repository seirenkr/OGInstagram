package main

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func shortcodePK(shortcode string) *big.Int {
	raw, err := base64.RawURLEncoding.DecodeString(strings.Repeat("A", (4-len(shortcode)%4)%4) + shortcode)
	if err != nil {
		return nil
	}
	return new(big.Int).SetBytes(raw)
}

const igEpochMs = 1314220021721

func shortcodeTime(shortcode string) time.Time {
	pk := shortcodePK(shortcode)
	if pk == nil || pk.Sign() == 0 || !pk.IsInt64() {
		return time.Time{}
	}
	t := time.UnixMilli((pk.Int64() >> 23) + igEpochMs).UTC()
	if t.After(time.Now()) {
		return time.Time{}
	}
	return t
}

func (a *App) postAvatarURL(baseURL string, post Post) string {
	if post.ProfilePic == "" {
		return baseURL + defaultAvatarPath
	}
	return a.offloadSigner.url(baseURL, "/offload/"+url.PathEscape(post.Shortcode)+"/avatar", false)
}

func (a *App) profileAvatarURL(baseURL string, p Profile) string {
	if p.ProfilePic == "" {
		return baseURL + defaultAvatarPath
	}
	path := "/offload/@" + url.PathEscape(strings.ToLower(p.Username)) + "/avatar"
	return a.offloadSigner.url(baseURL, path, false)
}

func (a *App) profileMediaOffloadURL(baseURL, username string, index int) string {
	path := "/offload/@" + url.PathEscape(strings.ToLower(username)) + "/" + strconv.Itoa(index+1)
	return a.offloadSigner.url(baseURL, path, false)
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

func normalizeCDNHost(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if strings.HasSuffix(u.Hostname(), ".fbcdn.net") || strings.HasSuffix(u.Hostname(), ".cdninstagram.com") {
		u.Host = "scontent.cdninstagram.com"
		return u.String()
	}
	return raw
}

func fmtCount(value int) string {
	if value < 0 {
		value = 0
	}
	s := strconv.Itoa(value)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func truncateFlat(text string, limit int) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	flat := strings.Join(lines, " ")

	r := []rune(flat)
	if len(r) <= limit {
		return flat
	}
	return strings.TrimRight(string(r[:limit-3]), " \t\n") + "..."
}

func normalizeCaption(text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func postDescription(caption, reaction string) string {
	nc := normalizeCaption(caption)
	tr := strings.TrimSpace(reaction)
	if tr != "" && nc != "" {
		return tr + "\n\n" + nc
	}
	if tr != "" {
		return tr
	}
	return nc
}

func isoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

const (
	cdnFallbackTTL = 24 * time.Hour
	cdnTTLMargin   = 30 * time.Minute
)

func cdnExpiry(rawURL string) (time.Time, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(u.Query().Get("oe"), 16, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

func cacheTTLFromURLs(urls ...string) time.Duration {
	ttl := cdnFallbackTTL
	for _, u := range urls {
		if u == "" {
			continue
		}
		if expiry, ok := cdnExpiry(u); ok {
			candidate := time.Until(expiry) - cdnTTLMargin
			if candidate < time.Minute {
				candidate = time.Minute
			}
			if candidate < ttl {
				ttl = candidate
			}
		}
	}
	return ttl
}

func cdnEdgeSeconds(urls ...string) int {
	return int(cacheTTLFromURLs(urls...) / time.Second)
}

func mediaIndexFor(post Post, requested int) int {
	if len(post.Attachments) == 0 || requested < 0 {
		return 0
	}
	if requested >= len(post.Attachments) {
		return len(post.Attachments) - 1
	}
	return requested
}

func instagramPostURL(postType, shortcode string, mediaIndex int, specified bool) string {
	target := instagramOrigin + "/" + normalizePostType(postType) + "/" + url.PathEscape(shortcode) + "/"
	if specified {
		idx := mediaIndex
		if idx < 0 {
			idx = 0
		}
		target += "?img_index=" + strconv.Itoa(idx+1)
	}
	return target
}

func (a *App) offloadURL(baseURL, shortcode string, index int, thumbnail bool) string {
	path := "/offload/" + url.PathEscape(shortcode) + "/" + strconv.Itoa(index+1)
	return a.offloadSigner.url(baseURL, path, thumbnail)
}

func videoDisplaySize(att Attachment) (int, int) {
	m := 1.0
	if att.Kind == "video" {
		if att.Width > 1920 || att.Height > 1920 {
			m = 0.5
		}
		if att.Width < 400 && att.Height < 400 {
			m = 2
		}
	}
	return int(float64(att.Width)*m + 0.5), int(float64(att.Height)*m + 0.5)
}

const galleryEmoji = "🖼️"

const activityMaxImages = 4

type attachmentSelection struct {
	items     []selectedAttachment
	indicator string
}

type selectedAttachment struct {
	att   Attachment
	index int
}

func selectActivityAttachments(post Post, mediaIndex int, specified bool) attachmentSelection {
	total := len(post.Attachments)
	if specified || total <= 1 {
		sel := mediaIndexFor(post, mediaIndex)
		return attachmentSelection{
			items:     []selectedAttachment{{post.Attachments[sel], sel}},
			indicator: singleAttachmentIndicator(post, sel),
		}
	}
	if post.Attachments[0].Kind == "video" {
		return attachmentSelection{
			items:     []selectedAttachment{{post.Attachments[0], 0}},
			indicator: galleryEmoji + " 1 / " + strconv.Itoa(total),
		}
	}
	var images []selectedAttachment
	for i, att := range post.Attachments {
		if att.Kind == "image" {
			images = append(images, selectedAttachment{att, i})
			if len(images) >= activityMaxImages {
				break
			}
		}
	}

	return attachmentSelection{items: images, indicator: galleryEmoji + " " + strconv.Itoa(total)}
}

func singleAttachmentIndicator(post Post, selectedIndex int) string {
	total := len(post.Attachments)
	if total > 1 {
		return galleryEmoji + " " + strconv.Itoa(selectedIndex+1) + " / " + strconv.Itoa(total)
	}
	return ""
}

func withIndicator(indicator, statsLine string) string {
	if indicator != "" {
		return indicator + "  " + statsLine
	}
	return statsLine
}
