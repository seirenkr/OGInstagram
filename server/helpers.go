package main

import (
	"encoding/json"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func shortcodePK(shortcode string) *big.Int {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	n := new(big.Int)
	base := big.NewInt(64)
	for _, ch := range shortcode {
		idx := strings.IndexRune(enc, ch)
		if idx < 0 {
			return nil
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	return n
}

// igEpochMs is the Instagram snowflake epoch, 2011-08-24T21:07:01.721Z.
const igEpochMs = 1314220021721

// shortcodeTime decodes the creation time embedded in a post shortcode: the
// media PK is a snowflake whose upper bits are milliseconds since the IG
// epoch. Zero time when the code doesn't decode to a plausible snowflake
// (invalid chars, empty, or the 24-char private-post codes).
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

// Avatars and profile media route through /offload like post media, so the
// served URL never carries an expiring CDN signature and stays same-origin.
func postAvatarURL(baseURL string, post Post) string {
	if post.ProfilePic == "" {
		return baseURL + defaultAvatarPath
	}
	return baseURL + "/offload/" + url.PathEscape(post.Shortcode) + "/avatar"
}

func profileAvatarURL(baseURL string, p Profile) string {
	if p.ProfilePic == "" {
		return baseURL + defaultAvatarPath
	}
	return baseURL + "/offload/@" + url.PathEscape(p.Username) + "/avatar"
}

func profileMediaOffloadURL(baseURL, username string, index int) string {
	return baseURL + "/offload/@" + url.PathEscape(username) + "/" + strconv.Itoa(index+1)
}

func jsonBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

var compactRE = regexp.MustCompile(`>\s+<`)

func compactHTML(v string) string {
	return compactRE.ReplaceAllString(strings.TrimSpace(v), "><")
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
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func truncateChars(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(text)
	if len(r) <= limit {
		return text
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	return strings.TrimRight(string(r[:limit-3]), " \t\n") + "..."
}

func truncateFlat(text string, limit int) string {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	return truncateChars(strings.Join(lines, " "), limit)
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

// cdnExpiry reads the "oe" query param (hex Unix expiry) from an Instagram CDN URL.
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

// cacheTTLFromURLs caches until the earliest CDN URL expiry (minus a margin), so
// a cached post never hands out a dead media URL. Falls back to cdnFallbackTTL
// when no URL carries an oe expiry.
func cacheTTLFromURLs(urls ...string) time.Duration {
	var earliest time.Time
	for _, u := range urls {
		if t, ok := cdnExpiry(u); ok && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		return cdnFallbackTTL
	}
	if ttl := time.Until(earliest) - cdnTTLMargin; ttl > time.Minute {
		return ttl
	}
	return time.Minute
}

// cdnEdgeSeconds converts the same oe-based TTL into an edge s-maxage, so a
// cached response embedding raw CDN URLs never outlives the media it points at.
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

func offloadURL(baseURL, shortcode string, index int, thumbnail bool) string {
	suffix := ""
	if thumbnail {
		suffix = "?thumbnail=1"
	}
	return baseURL + "/offload/" + url.PathEscape(shortcode) + "/" + strconv.Itoa(index+1) + suffix
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
