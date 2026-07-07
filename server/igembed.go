package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Instagram's public embed pages (/p/<code>/embed/captioned/ and /<user>/embed/)
// render the data server-side when the request does NOT look like a modern
// browser. They are served without a proxy, so we hit them via a.direct.
//
//   - Post:    /embed/captioned first, GraphQL (proxied) on failure.
//   - Profile: GraphQL (proxied) first, /embed/ on failure.
//
// Both pages carry the payload in a single JSON-encoded "contextJSON" string:
//   ...,"contextJSON":"{\"context\":{...},\"gql_data\":{\"shortcode_media\":{...}}}",...
// Decoding that one string yields plain JSON we read with gjson - no JS lexer,
// no HTML parser.

// directGet fetches url with no proxy and a non-browser UA (so IG server-renders
// the embed). It never touches the session pool, so it costs no proxy budget.
func (a *App) directGet(op, target, url string) (body string, ferr *AppError) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	defer func() {
		status := 200
		if ferr != nil {
			status = ferr.Status
		}
		logOutbound(op, target, "direct", http.MethodGet, url, started, status, len(body), ferr)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", igErr(500, "", err.Error())
	}
	req.Header.Set("User-Agent", embedUA)
	resp, err := a.direct.Do(req)
	if err != nil {
		return "", igErr(502, reasonConnection, err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != 200 {
		return "", igErr(resp.StatusCode, reasonClientError, http.StatusText(resp.StatusCode))
	}
	return string(raw), nil
}

// embedContextJSON pulls the inner JSON out of the escaped "contextJSON" string.
func embedContextJSON(html string) (string, *AppError) {
	const key = `"contextJSON":`
	i := strings.Index(html, key)
	if i < 0 {
		return "", igErr(502, reasonClientError, "embed contextJSON not found")
	}
	var inner string
	if err := json.NewDecoder(strings.NewReader(html[i+len(key):])).Decode(&inner); err != nil || inner == "" {
		return "", igErr(502, reasonClientError, "embed contextJSON decode failed")
	}
	return inner, nil
}

func (a *App) fetchPostEmbed(shortcode string) (Post, *AppError) {
	html, err := a.directGet("post", shortcode, instagramOrigin+"/p/"+url.PathEscape(shortcode)+"/embed/captioned/")
	if err != nil {
		return Post{}, err
	}
	return parseEmbedPost(html)
}

func parseEmbedPost(page string) (Post, *AppError) {
	inner, err := embedContextJSON(page)
	if err != nil {
		return parseEmbedSimple(page) // contextJSON null → DOM-rendered simple variant
	}
	sm := gjson.Get(inner, "gql_data.shortcode_media")
	if !present(sm) {
		return Post{}, igErr(502, reasonClientError, "embed missing media")
	}
	return parseGraphMedia(sm)
}

var (
	simpleMediaTypeRE   = regexp.MustCompile(`data-media-type="([^"]*)"`)
	simpleOwnerIDRE     = regexp.MustCompile(`data-owner-id="([^"]*)"`)
	simpleMediaIDRE     = regexp.MustCompile(`data-media-id="([^"]*)"`)
	simplePermalinkRE   = regexp.MustCompile(`data-permalink="[^"]*?/p/([A-Za-z0-9_-]+)`)
	simpleUsernameRE    = regexp.MustCompile(`class="UsernameText">([^<]*)<`)
	simpleAvatarRE      = regexp.MustCompile(`(?s)class="Avatar[^"]*"[^>]*>\s*<img[^>]*\bsrc="([^"]*)"`)
	simpleImageTagRE    = regexp.MustCompile(`(?s)<img class="EmbeddedMediaImage"[^>]*>`)
	simpleSrcRE         = regexp.MustCompile(`\bsrc="([^"]*)"`)
	simpleSrcsetRE      = regexp.MustCompile(`\bsrcset="([^"]*)"`)
	simpleFrameRatioRE  = regexp.MustCompile(`EmbedFrame"[^>]*padding-bottom:\s*([\d.]+)%`)
	simpleLikesRE       = regexp.MustCompile(`>([\d,]+)\s+likes<`)
	simpleCommentsRE    = regexp.MustCompile(`([\d,]+)\s+comments`)
	simpleCaptionOpenRE = regexp.MustCompile(`<div class="Caption">`)
	simpleCaptionUserRE = regexp.MustCompile(`(?s)^\s*<a class="CaptionUsername"[^>]*>[^<]*</a>`)
	simpleBrRE          = regexp.MustCompile(`(?i)<br\s*/?>`)
	simpleTagRE         = regexp.MustCompile(`<[^>]+>`)
)

// parseEmbedSimple parses the PolarisEmbedSimple DOM (contextJSON null): single
// image only; video/carousel/missing error out so the caller falls back to GraphQL.
func parseEmbedSimple(page string) (Post, *AppError) {
	mediaType := firstGroup(simpleMediaTypeRE, page)
	if mediaType == "" {
		return Post{}, igErr(502, reasonClientError, "simple embed: no media node")
	}
	if !strings.Contains(mediaType, "Image") {
		return Post{}, igErr(502, reasonClientError, "simple embed: unsupported media type "+mediaType)
	}
	username := firstGroup(simpleUsernameRE, page)
	if username == "" {
		return Post{}, igErr(502, reasonClientError, "simple embed missing owner")
	}

	imgTag := simpleImageTagRE.FindString(page)
	imgURL, w := bestSrcset(firstGroup(simpleSrcsetRE, imgTag))
	if imgURL == "" {
		imgURL = html.UnescapeString(firstGroup(simpleSrcRE, imgTag))
	}
	if imgURL == "" {
		return Post{}, igErr(502, reasonClientError, "simple embed had no image")
	}
	imgURL = normalizeCDNHost(imgURL)

	h := 0
	if ratio, err := strconv.ParseFloat(firstGroup(simpleFrameRatioRE, page), 64); err == nil && w > 0 {
		h = int(float64(w) * ratio / 100)
	}

	att := Attachment{
		ID:        firstGroup(simpleMediaIDRE, page),
		Kind:      "image",
		URL:       imgURL,
		Thumbnail: imgURL,
		Width:     w,
		Height:    h,
	}

	return Post{
		Shortcode:  firstGroup(simplePermalinkRE, page),
		Username:   username,
		OwnerID:    firstGroup(simpleOwnerIDRE, page),
		FullName:   "",
		ProfilePic: normalizeCDNHost(html.UnescapeString(firstGroup(simpleAvatarRE, page))),
		Caption:    simpleCaption(page),
		StatsLine: "❤️ " + fmtCount(parseCount(firstGroup(simpleLikesRE, page))) +
			"  \U0001f4ac " + fmtCount(parseCount(firstGroup(simpleCommentsRE, page))),
		Attachments: []Attachment{att},
	}, nil
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func parseCount(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}

// bestSrcset returns the widest srcset entry's URL and width (0 when the
// entries carry no width descriptor).
func bestSrcset(srcset string) (string, int) {
	best, bestW := "", -1
	for _, part := range strings.Split(srcset, ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) == 0 {
			continue
		}
		w := 0
		if len(f) > 1 {
			w, _ = strconv.Atoi(strings.TrimSuffix(f[len(f)-1], "w"))
		}
		if best == "" || w > bestW {
			best, bestW = html.UnescapeString(f[0]), w
		}
	}
	return best, max(bestW, 0)
}

func simpleCaption(page string) string {
	loc := simpleCaptionOpenRE.FindStringIndex(page)
	if loc == nil {
		return ""
	}
	rest := page[loc[1]:]
	end := len(rest)
	if i := strings.Index(rest, `<div class="CaptionComments">`); i >= 0 {
		end = i
	} else if i := strings.Index(rest, "</div>"); i >= 0 {
		end = i
	}
	body := simpleCaptionUserRE.ReplaceAllString(rest[:end], "")
	body = simpleBrRE.ReplaceAllString(body, "\n")
	body = simpleTagRE.ReplaceAllString(body, "")
	return normalizeCaption(html.UnescapeString(body))
}

// parseGraphMedia reads the old-style GraphQL "shortcode_media" node the embed
// exposes. A video node without a real video_url means the embed blocked it
// (WatchOnInstagram); we return an error so the caller falls back to GraphQL.
func parseGraphMedia(sm gjson.Result) (Post, *AppError) {
	owner := sm.Get("owner")
	username := owner.Get("username").String()
	if username == "" {
		return Post{}, igErr(502, reasonClientError, "embed missing owner")
	}

	var atts []Attachment
	var blocked bool
	add := func(n gjson.Result) {
		att, ok, b := graphAttachment(n)
		if b {
			blocked = true
		} else if ok {
			atts = append(atts, att)
		}
	}
	if kids := sm.Get("edge_sidecar_to_children.edges"); len(kids.Array()) > 0 {
		kids.ForEach(func(_, e gjson.Result) bool {
			add(e.Get("node"))
			return !blocked
		})
	} else {
		add(sm)
	}
	if blocked {
		return Post{}, igErr(502, reasonClientError, "embed video blocked")
	}
	if len(atts) == 0 {
		return Post{}, igErr(502, reasonClientError, "embed had no attachments")
	}

	return Post{
		Shortcode:  sm.Get("shortcode").String(),
		Username:   username,
		OwnerID:    owner.Get("id").String(),
		FullName:   owner.Get("full_name").String(),
		ProfilePic: normalizeCDNHost(owner.Get("profile_pic_url").String()),
		Caption:    sm.Get("edge_media_to_caption.edges.0.node.text").String(),
		StatsLine: "❤️ " + fmtCount(uintOf(sm, "edge_liked_by.count")) +
			"  \U0001f4ac " + fmtCount(uintOf(sm, "edge_media_to_comment.count")),
		Attachments: atts,
		// CreatedAt left zero: embed carries no timestamp. fetchPost fills it
		// from the shortcode snowflake; consumers null-handle a remaining zero.
	}, nil
}

// graphAttachment returns (attachment, ok, blocked). blocked is true only for a
// video whose video_url the embed withheld.
func graphAttachment(n gjson.Result) (Attachment, bool, bool) {
	img := normalizeCDNHost(bestGraphImageURL(n))
	if img == "" {
		return Attachment{}, false, false
	}
	w, h := uintOf(n, "dimensions.width"), uintOf(n, "dimensions.height")
	id := firstNonEmpty(n.Get("id").String(), n.Get("pk").String())
	if n.Get("is_video").Bool() {
		u := n.Get("video_url").String()
		if u == "" {
			return Attachment{}, false, true
		}
		return Attachment{ID: id, Kind: "video", URL: normalizeCDNHost(u), Thumbnail: img, Width: w, Height: h}, true, false
	}
	return Attachment{ID: id, Kind: "image", URL: img, Thumbnail: img, Width: w, Height: h}, true, false
}

func bestGraphImageURL(n gjson.Result) string {
	if u := n.Get("display_url").String(); u != "" {
		return u
	}
	return bestCandidateURL(n.Get("display_resources"))
}

func (a *App) fetchProfileEmbed(username string) (Profile, *AppError) {
	html, err := a.directGet("profile", username, instagramOrigin+"/"+url.PathEscape(username)+"/embed/")
	if err != nil {
		return Profile{}, err
	}
	return parseEmbedProfile(html)
}

func parseEmbedProfile(html string) (Profile, *AppError) {
	inner, err := embedContextJSON(html)
	if err != nil {
		return Profile{}, err
	}
	ctx := gjson.Get(inner, "context")
	username := ctx.Get("username").String()
	if username == "" {
		return Profile{}, igErr(404, reasonNotFound, "embed profile missing username")
	}
	p := Profile{
		Username:      username,
		UserID:        ctx.Get("owner_id").String(),
		FullName:      ctx.Get("full_name").String(),
		ProfilePic:    normalizeCDNHost(ctx.Get("profile_pic_url").String()),
		FollowerCount: uintOf(ctx, "followers_count"),
		MediaCount:    uintOf(ctx, "posts_count"),
		IsPrivate:     ctx.Get("is_private").Bool(), // ponytail: absent for public; GraphQL path is authoritative for private
	}
	ctx.Get("graphql_media").ForEach(func(_, m gjson.Result) bool {
		sm := m.Get("shortcode_media")
		thumb := normalizeCDNHost(bestGraphImageURL(sm))
		if thumb != "" {
			p.RecentMedia = append(p.RecentMedia, ProfileMedia{
				ID:        firstNonEmpty(sm.Get("id").String(), sm.Get("shortcode").String()),
				Thumbnail: thumb,
				Width:     uintOf(sm, "dimensions.width"),
				Height:    uintOf(sm, "dimensions.height"),
			})
		}
		return len(p.RecentMedia) < profileGalleryMax
	})
	return p, nil
}
