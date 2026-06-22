package main

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"\"", "&quot;",
	"'", "&#39;",
)

func htmlEscape(v string) string { return htmlEscaper.Replace(v) }

func attr(v string) string { return htmlEscape(v) }

var compactRE = regexp.MustCompile(`>\s+<`)

func compactHTML(v string) string {
	return compactRE.ReplaceAllString(strings.TrimSpace(v), "><")
}

func pathEscape(v string) string { return url.PathEscape(v) }

func normalizeCDNHost(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if strings.HasSuffix(u.Host, ".fbcdn.net") {
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
	return strings.Replace(t.UTC().Format("2006-01-02T15:04:05.000Z"), ".000Z", "Z", 1)
}

func nowUTC() time.Time   { return time.Now().UTC() }
func epochUTC() time.Time { return time.Unix(0, 0).UTC() }

func mediaIndexFor(post Post, requested int) int {
	if len(post.Attachments) == 0 || requested < 0 {
		return 0
	}
	if requested >= len(post.Attachments) {
		return len(post.Attachments) - 1
	}
	return requested
}

func instagramURLForSelection(postType, shortcode string, mediaIndex int, specified bool) string {
	target := instagramOrigin + "/" + normalizePostType(postType) + "/" + pathEscape(shortcode) + "/"
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
	return baseURL + "/offload/" + pathEscape(shortcode) + "/" + strconv.Itoa(index+1) + suffix
}

func optionalString(v string) string { return strings.TrimSpace(v) }

const galleryEmoji = "🖼️"

const activityMaxImages = 3

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
	if len(images) == total {
		return attachmentSelection{items: images, indicator: galleryEmoji + " " + strconv.Itoa(total)}
	}
	return attachmentSelection{items: images, indicator: galleryEmoji + " " + strconv.Itoa(len(images)) + " / " + strconv.Itoa(total)}
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
