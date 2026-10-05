package main

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Discord's draft is a link-preview payload, not a webhook message:
// https://github.com/discord/discord-api-docs/pull/8606
// The 3,000-byte limit includes JSON/HTML escapes and every signed media URL.
const discordComponentMaxBytes = 3000

type discordEmbedVariant uint8

const (
	discordLegacy discordEmbedVariant = iota
	discordComponents
	discordComponentsSupport
)

type discordMedia struct {
	URL string `json:"url"`
}

type discordGalleryItem struct {
	Media discordMedia `json:"media"`
}

// Only the display components we produce. No ids, custom_ids or interactions.
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
	name, username, authorURL, avatar, origin, caption string
	stats, footer, verifiedEmoji                       string
	media                                              []string
	gallery, support, verified                         bool
}

func discordRequestVariant(req *http.Request) discordEmbedVariant {
	if !strings.Contains(req.UserAgent(), "Discordbot") {
		return discordLegacy
	}
	// Testing override: ?e=c pins the Component Embed and ?e=s adds the support
	// button. Discord caches per URL, so add any other parameter (&n=2) to refetch.
	switch req.URL.Query().Get("e") {
	case "c":
		return discordComponents
	case "s":
		return discordComponentsSupport
	}
	// Split requests, not posts: a cached model can serve either variant. The
	// gateway gives each request a Cloudflare Ray ID or a fresh random ID.
	id, _ := req.Context().Value(requestIDKey{}).(string)
	if id == "" {
		id = rand.Text()
	}
	digest := sha256.Sum256([]byte(id))
	if digest[0]&1 != 0 {
		return discordLegacy
	}
	// Use independent hash bytes for the 50% support-button sample within the
	// Component Embed arm; fitting the payload must not reroll the choice.
	if digest[1]&1 == 0 {
		return discordComponentsSupport
	}
	return discordComponents
}

func discordEmbedResponse(req *http.Request, document string, choice discordEmbedVariant) resp {
	r := htmlResp(http.StatusOK, document)
	if !strings.Contains(req.UserAgent(), "Discordbot") {
		return r
	}
	variant := "mastodon"
	if choice != discordLegacy {
		variant = "component"
	}
	rendered := "opengraph"
	if strings.Contains(document, `id="discord:component-embed"`) {
		rendered = "component"
	} else if strings.Contains(document, `type="application/activity+json"`) {
		rendered = "mastodon"
	}
	r.headers["X-OGInstagram-Embed"] = rendered
	logger(req.Context()).Info("discord embed served", "variant", variant, "rendered", rendered, "path", req.URL.Path)
	return r
}

var discordCountRE = regexp.MustCompile(`[0-9][0-9,]*`)

func discordMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// Escape only what Discord parses mid-line; each backslash costs two JSON
// bytes. "://" blocks auto-links and "@\u200b" blocks mention syntax.
var discordMarkdownEscaper = strings.NewReplacer(
	"\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|",
	"[", "\\[", "]", "\\]", "<", "\\<", "://", "\\://", "@", "@\u200b",
)

// Only use this in ordinary Text Display content. Discord's masked-link
// labels disable the escape rule, so these backslashes would become visible.
func discordText(text string) string {
	lines := strings.Split(discordMarkdownEscaper.Replace(text), "\n")
	for i, line := range lines {
		// Headings, subtext, lists and quotes start only at the beginning of a line.
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
		return "<" + target + ">" // A bare URL needs no label; <> bounds the auto-link.
	}
	// Link labels have their own formatting parser and strip invisible Unicode
	// characters. Use raw labels only when both steps preserve the original.
	unsafe := strings.ContainsAny(label, "\\[]*~`<>") || strings.Contains(label, "||") || strings.Contains(label, "://") ||
		strings.Contains(label, "__") || discordUnderscoreCanOpen(label) ||
		strings.ContainsAny(label, "\u034f\u17b4\u17b5\u1160\u3164\uffa0") ||
		strings.IndexFunc(label, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ')
		}) >= 0
	if unsafe {
		// Keep the original text outside the link instead of changing its visible
		// characters. Escaping '.' and ':' also prevents unintended auto-links.
		return discordText(label) + " [↗](" + target + ")"
	}
	return "[" + label + "](" + target + ")"
}

// Discord italicizes _x_ only when an underscore after a non-word character
// pairs with the next underscore before a non-word character (or the end).
// A lone or in-word underscore (alice_photos, a._b) stays literal.
func discordUnderscoreCanOpen(label string) bool {
	open := -1
	for i := 0; i < len(label); i++ {
		if label[i] != '_' {
			continue
		}
		if open >= 0 && i > open+1 && (i == len(label)-1 || !isWordByte(label[i+1])) {
			return true
		}
		open = -1
		if i == 0 || !isWordByte(label[i-1]) {
			open = i
		}
	}
	return false
}

// A JavaScript regex \w character, which is what Discord's \b boundaries use.
func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Find links in the original caption before fitting it to the byte budget. A
// partially displayed URL must not become a link to a different destination.
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
	if len(id) < 17 || len(id) > 20 || strings.Trim(id, "0123456789") != "" {
		return false
	}
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n != 0
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
		name: post.FullName, username: post.Username, authorURL: profileURL(post.Username),
		avatar: a.postAvatarURL(baseURL, post), origin: instagramPostURL(postType, post.Shortcode, index, specified),
		caption: post.Caption, stats: discordText(truncateFlat(post.StatsLine, 128)),
		footer: discordFooter(post.CreatedAt, a.cfg.DiscordBrandEmojiID), gallery: gallery, support: support,
		verified: post.IsVerified, verifiedEmoji: discordEmoji("verified", a.cfg.DiscordVerifiedEmojiID),
	}
	for i := range post.Attachments {
		if !specified || i == index {
			card.media = append(card.media, a.offloadURL(baseURL, post.Shortcode, i, false))
		}
		if len(card.media) == 10 {
			break
		}
	}
	return componentEmbedScript(card)
}

func (a *App) profileComponentEmbed(baseURL string, p Profile, gallery, support bool) string {
	card := discordCard{
		name: p.FullName, username: p.Username, authorURL: profileURL(p.Username),
		avatar: a.profileAvatarURL(baseURL, p), origin: profileURL(p.Username),
		caption: p.Biography, stats: discordText(profileStatsLine(p)),
		footer: discordFooter(time.Time{}, a.cfg.DiscordBrandEmojiID), gallery: gallery, support: support,
		verified: p.IsVerified, verifiedEmoji: discordEmoji("verified", a.cfg.DiscordVerifiedEmojiID),
	}
	if p.IsPrivate {
		card.stats += "\n-# " + discordText(profilePrivateNotice)
	}
	for i := range p.RecentMedia[:min(len(p.RecentMedia), 10)] {
		card.media = append(card.media, a.profileMediaOffloadURL(baseURL, p.Username, i))
	}
	if gallery && len(card.media) == 0 {
		card.media = []string{card.avatar}
	}
	return componentEmbedScript(card)
}

func (a *App) storyComponentEmbed(baseURL string, story Story, gallery, support bool) string {
	return componentEmbedScript(discordCard{
		name: story.FullName, username: story.Username, authorURL: profileURL(story.Username),
		avatar: a.storyAvatarURL(baseURL, story), origin: storyOriginURL(story.Username, story.ID),
		caption: story.Caption, footer: discordFooter(story.CreatedAt, a.cfg.DiscordBrandEmojiID), gallery: gallery, support: support,
		verified: story.IsVerified, verifiedEmoji: discordEmoji("verified", a.cfg.DiscordVerifiedEmojiID),
		media: []string{a.storyOffloadURL(baseURL, story.Username, story.ID, false)},
	})
}

// json.Marshal's HTML escaping spends 6 bytes on every "&" in signed media
// URLs. Only "<" can start markup inside the script, so escape just that.
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
	if !discordMediaURL(card.origin) || !discordMediaURL(card.authorURL) {
		return ""
	}
	var items []discordGalleryItem
	for _, mediaURL := range card.media {
		if !discordMediaURL(mediaURL) {
			return "" // Keep the established embed if we cannot describe its media.
		}
		items = append(items, discordGalleryItem{Media: discordMedia{URL: mediaURL}})
		if len(items) == 10 {
			break
		}
	}
	// Discord left some names (emoji-heavy ones, at least) as raw [label](url)
	// in headings, so the name stays plain text and the ASCII handle carries the link.
	// Name, handle and stats share one size; weight and link color set them apart.
	author := "**" + discordText(truncateFlat(cmp.Or(card.name, card.username), 120)) + "**"
	if card.verified && card.verifiedEmoji != "" {
		author += " " + card.verifiedEmoji
	}
	if card.username != "" {
		author += "\n" + discordLink("@"+truncateFlat(card.username, 30), card.authorURL)
	}
	if !card.gallery && card.stats != "" {
		// A third line fills the fixed-size avatar thumbnail's height.
		// Full-size line with bold counts so stats read as content, not metadata.
		// A blank line separates who posted from the post's numbers.
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
			instagram := discordComponent{Type: 2, Style: 5, Label: "📷 Instagram", URL: card.origin}
			brand := discordComponent{Type: 10, Content: "-# " + card.footer}
			if card.support {
				// Two buttons pair up in their own row below a divider.
				children = append(children, brand, discordComponent{Type: 14}, discordComponent{Type: 1, Components: []discordComponent{
					instagram, {Type: 2, Style: 5, Label: "☕ Support me", URL: "https://ko-fi.com/seirenkr"}}})
			} else {
				// The lone post button sits beside the brand line as the Section accessory.
				children = append(children, discordComponent{Type: 9, Components: []discordComponent{brand}, Accessory: &instagram})
			}
		}
		// At most 11 components including the sole top-level container. The fixed
		// layout stays below Discord's 40-component and 4,000-character limits.
		payload := struct {
			Component discordComponent `json:"component"`
		}{discordComponent{Type: 17, AccentColor: 0xff0069, Components: children}}
		return discordJSON(payload)
	}

	body := encode("")
	// Preserve media ahead of caption text. Long hosts/key IDs may require a
	// smaller gallery; do not send an invalid payload and lose the whole unfurl.
	for len(body) > discordComponentMaxBytes && len(items) > 1 {
		items = items[:len(items)-1]
		body = encode("")
	}
	if len(body) == 0 || len(body) > discordComponentMaxBytes {
		return ""
	}
	if !card.gallery {
		caption := normalizeCaption(card.caption)
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
