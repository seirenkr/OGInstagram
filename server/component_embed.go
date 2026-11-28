package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// https://docs.discord.com/developers/link-previews/component-embeds (3,000-byte payload cap).
const discordComponentMaxBytes = 3000

type discordMedia struct {
	URL string `json:"url"`
}

type discordGalleryItem struct {
	Media discordMedia `json:"media"`
}

type discordComponent struct {
	Type        int                  `json:"type"`
	Content     string               `json:"content,omitempty"`
	Components  []discordComponent   `json:"components,omitempty"`
	Accessory   *discordComponent    `json:"accessory,omitempty"`
	Media       *discordMedia        `json:"media,omitempty"`
	Items       []discordGalleryItem `json:"items,omitempty"`
	AccentColor int                  `json:"accent_color,omitempty"`
	Style       int                  `json:"style,omitempty"`
	URL         string               `json:"url,omitempty"`
	Label       string               `json:"label,omitempty"`
}

type discordCard struct {
	name, username, avatar, origin, caption string
	stats, footer, verifiedEmoji            string
	media                                   []string
	gallery, support, verified              bool
}

func discordSupport(req *http.Request) bool {
	return req.URL.Query().Get("e") == "s" || mathrand.IntN(20) == 0
}

func cardResp(document, id, actor string) resp {
	r := resp{status: http.StatusOK, body: []byte(document), headers: map[string]string{
		"Content-Type": "text/html; charset=utf-8", "Vary": "Accept", "X-OGInstagram-Embed": "opengraph"}}
	if id != "" {
		r.headers["Link"] = apLinks(id, asMediaType, actor)
	}
	if strings.Contains(document, `id="discord:component-embed"`) {
		r.headers["X-OGInstagram-Embed"] = "component"
	}
	return r
}

var discordCountRE = regexp.MustCompile(`[0-9][0-9,.]*[KMB]?`)

func discordMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

var discordMarkdownEscaper = strings.NewReplacer(
	"\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|",
	"[", "\\[", "]", "\\]", "<", "\\<", "://", "\\://", "@", "@\u200b",
)

// Do not use inside Discord masked-link labels, which render backslashes literally.
func discordText(text string) string {
	lines := strings.Split(discordMarkdownEscaper.Replace(text), "\n")
	for i, line := range lines {
		if line != "" && strings.IndexByte("#-+>", line[0]) >= 0 {
			lines[i] = "\\" + line
		} else if n := len(line) - len(strings.TrimLeft(line, "0123456789")); n > 0 && strings.HasPrefix(line[n:], ".") {
			lines[i] = line[:n] + "\\" + line[n:]
		}
	}
	return strings.Join(lines, "\n")
}

var discordLinkEscaper = strings.NewReplacer(
	"(", "%28", ")", "%29", "\\", "%5C", "[", "%5B", "]", "%5D", "<", "%3C", ">", "%3E",
)

func discordLink(label, target string) string {
	target = discordLinkEscaper.Replace(target)
	if label == target && (strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://")) {
		return "<" + target + ">"
	}
	unsafe := strings.ContainsAny(label, "\\[]") || strings.Contains(label, "://") ||
		strings.ContainsAny(label, "\u034f\u17b4\u17b5\u1160\u3164\uffa0") ||
		strings.IndexFunc(label, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ')
		}) >= 0
	if unsafe {
		return discordText(label) + " [↗](" + target + ")"
	}
	return "[" + label + "](" + target + ")"
}

func discordCaption(text string, spans []linkSpan, end int) string {
	var b strings.Builder
	last := 0
	for _, s := range spans {
		if s.start >= end {
			break
		}
		if s.end > end {
			end = s.start
			break
		}
		if !discordMediaURL(s.href) {
			continue
		}
		b.WriteString(discordText(text[last:s.start]))
		b.WriteString(discordLink(text[s.start:s.end], s.href))
		last = s.end
	}
	b.WriteString(discordText(text[last:end]))
	if end < len(text) {
		b.WriteString("…")
	}
	return b.String()
}

func validDiscordEmojiID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return len(id) >= 17 && len(id) <= 20 && err == nil && n != 0
}

func discordEmoji(name, id string) string {
	if !validDiscordEmojiID(id) {
		return ""
	}
	return "<:" + name + ":" + id + ">"
}

func discordFooter(created time.Time, emojiID string) string {
	footer := brandName
	if emoji := discordEmoji("OGInstagram", emojiID); emoji != "" {
		footer = emoji + " " + footer
	}
	if !created.IsZero() {
		footer += " · <t:" + strconv.FormatInt(created.Unix(), 10) + ":s>"
	}
	return footer
}

func (a *App) postComponentEmbed(baseURL string, post Post, postType string, mediaIndex int, specified, gallery, support bool) string {
	index := mediaIndexFor(post, mediaIndex)
	card := discordCard{
		name: post.FullName, username: post.Username,
		avatar: a.postAvatarURL(baseURL, post), origin: instagramPostURL(postType, post.Shortcode, index, specified),
		caption: post.Caption, stats: discordText(truncateFlat(post.StatsLine, 128)),
		footer: discordFooter(post.CreatedAt, a.cfg.DiscordBrandEmojiID), gallery: gallery, support: support,
		verified: post.IsVerified, verifiedEmoji: discordEmoji("verified", a.cfg.DiscordVerifiedEmojiID),
	}
	for i := range post.Attachments {
		if !specified || i == index {
			card.media = append(card.media, a.offloadURL(baseURL, post.Shortcode, i, false))
		}
	}
	return componentEmbedScript(card)
}

func (a *App) profileComponentEmbed(baseURL string, p Profile, gallery, support bool) string {
	card := discordCard{
		name: p.FullName, username: p.Username,
		avatar: a.profileAvatarURL(baseURL, p), origin: profileURL(p.Username),
		caption: p.Biography, stats: discordText(profileStatsLine(p)),
		footer: discordFooter(time.Time{}, a.cfg.DiscordBrandEmojiID), gallery: gallery, support: support,
		verified: p.IsVerified, verifiedEmoji: discordEmoji("verified", a.cfg.DiscordVerifiedEmojiID),
	}
	if p.IsPrivate {
		card.stats += "\n-# " + discordText(profilePrivateNotice)
	}
	for i := range p.RecentMedia {
		card.media = append(card.media, a.profileMediaOffloadURL(baseURL, p.Username, i))
	}
	if gallery && len(card.media) == 0 {
		card.media = []string{card.avatar}
	}
	return componentEmbedScript(card)
}

func (a *App) storyComponentEmbed(baseURL string, story Story, gallery, support bool) string {
	return componentEmbedScript(discordCard{
		name: story.FullName, username: story.Username,
		avatar: a.storyAvatarURL(baseURL, story), origin: storyOriginURL(story.Username, story.ID),
		caption: story.Caption, footer: discordFooter(story.CreatedAt, a.cfg.DiscordBrandEmojiID), gallery: gallery, support: support,
		verified: story.IsVerified, verifiedEmoji: discordEmoji("verified", a.cfg.DiscordVerifiedEmojiID),
		media: []string{a.storyOffloadURL(baseURL, story.Username, story.ID, false)},
	})
}

// Avoid escaping "&" (6 bytes each in signed URLs); only "<" can break out of <script>.
func discordJSON(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil
	}
	return bytes.ReplaceAll(bytes.TrimSuffix(b.Bytes(), []byte("\n")), []byte("<"), []byte(`\u003c`))
}

func componentEmbedScript(card discordCard) string {
	var items []discordGalleryItem
	for _, mediaURL := range card.media {
		if !discordMediaURL(mediaURL) {
			return ""
		}
		items = append(items, discordGalleryItem{Media: discordMedia{URL: mediaURL}})
		if len(items) == 10 {
			break
		}
	}
	// Discord leaves emoji-heavy names unparsed in masked links, so the handle carries the link.
	author := "**" + discordText(cmp.Or(truncateFlat(card.name, 120), card.username)) + "**"
	if card.verified && card.verifiedEmoji != "" {
		author += " " + card.verifiedEmoji
	}
	if validUsername(card.username) {
		author += "\n[@" + card.username + "](" + profileURL(card.username) + ")"
	}
	if !card.gallery && card.stats != "" {
		author += "\n\n" + discordCountRE.ReplaceAllString(card.stats, "**$0**")
	}
	header := discordComponent{Type: 10, Content: author}
	if discordMediaURL(card.avatar) {
		header = discordComponent{Type: 9, Components: []discordComponent{header},
			Accessory: &discordComponent{Type: 11, Media: &discordMedia{URL: card.avatar}}}
	}
	encode := func(caption string) []byte {
		children := []discordComponent{header}
		if !card.gallery && caption != "" {
			children = append(children, discordComponent{Type: 10, Content: caption})
		}
		if len(items) != 0 {
			children = append(children, discordComponent{Type: 12, Items: items})
		}
		if !card.gallery {
			instagram := discordComponent{Type: 2, Style: 5, Label: "Instagram", URL: card.origin}
			brand := discordComponent{Type: 10, Content: "-# " + card.footer}
			if card.support {
				children = append(children, brand, discordComponent{Type: 14}, discordComponent{Type: 1, Components: []discordComponent{
					instagram, {Type: 2, Style: 5, Label: "Support me", URL: "https://ko-fi.com/seirenkr"}}})
			} else {
				children = append(children, discordComponent{Type: 9, Components: []discordComponent{brand}, Accessory: &instagram})
			}
		}
		payload := struct {
			Component discordComponent `json:"component"`
		}{discordComponent{Type: 17, AccentColor: 0xff0069, Components: children}}
		return discordJSON(payload)
	}

	body := encode("")
	for len(body) > discordComponentMaxBytes && len(items) > 1 {
		items = items[:len(items)-1]
		body = encode("")
	}
	if len(body) == 0 || len(body) > discordComponentMaxBytes {
		return ""
	}
	if !card.gallery {
		caption := strings.ToValidUTF8(normalizeCaption(card.caption), "\uFFFD")
		spans := detectLinks(caption)
		full := encode(discordCaption(caption, spans, len(caption)))
		if len(full) <= discordComponentMaxBytes {
			body = full
		} else {
			runes := []rune(caption)
			lo, hi := 0, min(len(runes)-1, 4000)
			for lo < hi {
				mid := lo + (hi-lo+1)/2
				candidate := encode(discordCaption(caption, spans, len(string(runes[:mid]))))
				if len(candidate) <= discordComponentMaxBytes {
					lo, body = mid, candidate
				} else {
					hi = mid - 1
				}
			}
		}
	}
	return `<script id="discord:component-embed" type="application/vnd.discord.component-embed+json">` + string(body) + `</script>`
}
