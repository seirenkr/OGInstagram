package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

func (a *App) directGet(ctx context.Context, operation, rawURL string) (string, *AppError) {
	return a.directFetch(ctx, fetchSpec{operation: operation, url: rawURL})
}

// directFetch GETs spec.url from this host with the crawler UA.
func (a *App) directFetch(ctx context.Context, spec fetchSpec) (out string, ferr *AppError) {
	started := time.Now()
	status := 0
	defer func() {
		logOutbound(ctx, spec.operation, "direct", http.MethodGet, spec.url, started, status, len(out), ferr, false)
	}()
	client := *http.DefaultClient
	loginRedirect := errors.New("Instagram redirected to login or verification")
	loginStatus := 0
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if isInstagramURL(req.URL.String()) {
			for _, path := range []string{"/accounts/login", "/challenge", "/checkpoint"} {
				if req.URL.Path == path || strings.HasPrefix(req.URL.Path, path+"/") {
					if req.Response != nil {
						loginStatus = req.Response.StatusCode
					}
					return loginRedirect
				}
			}
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	spec.method, spec.subject, spec.interpret = http.MethodGet, "Instagram", statusOnly
	spec.headers = map[string]string{"User-Agent": embedUA}
	status, out, ferr = fetchRequest(ctx, &client, spec)
	if ferr != nil && errors.Is(ferr.Cause, loginRedirect) {
		status = loginStatus
		ferr = igErr(http.StatusBadGateway, errorCodeLoginRequired, loginRedirect.Error())
	}
	return
}

const rulingAsbdID = "359341"

// mediaMissing asks Instagram's content-ruling endpoint (~200 B) whether a post
// exists for logged-out viewers. Only a definite "Media cannot be found" counts
// as missing; answered is false when errors or timeouts left the question open.
func (a *App) mediaMissing(ctx context.Context, shortcode string) (missing, answered bool) {
	pk := shortcodePK(shortcode)
	if pk == nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(ctx, rulingTimeout)
	defer cancel()
	_, out, ferr := a.fetchViaProxy(ctx, fetchSpec{
		operation:  "ruling",
		method:     http.MethodGet,
		url:        instagramOrigin + "/api/v1/web/get_ruling_for_content/?content_type=MEDIA&target_id=" + pk.String(),
		subject:    "Instagram",
		expectJSON: true,
		advisory:   true,
		headers: map[string]string{
			"User-Agent": "Mozilla/5.0", "X-IG-App-ID": instagramAppID, "X-ASBD-ID": rulingAsbdID, "X-IG-WWW-Claim": "0",
		},
		// "Media cannot be found" arrives as 404 with a JSON body.
		interpret: func(status int, raw []byte) *AppError {
			if status == http.StatusNotFound {
				return nil
			}
			return statusOnly(status, raw)
		},
	})
	return ferr == nil && gjson.Get(out, "status").String() == "fail" && gjson.Get(out, "message").String() == "Media cannot be found", ferr == nil
}

func embedContextJSON(page string) (string, *AppError) {
	const key = `"contextJSON":`
	i := strings.Index(page, key)
	if i < 0 {
		return "", igErr(502, errorCodeUpstream, "embed contextJSON not found")
	}
	var inner string
	if err := json.NewDecoder(strings.NewReader(page[i+len(key):])).Decode(&inner); err != nil || inner == "" {
		return "", igErr(502, errorCodeUpstream, "embed contextJSON decode failed")
	}
	return inner, nil
}

// Returned as is, never copied or mutated: stagedFetch tells a skip from a
// failure by this pointer.
var errEmbedPaused = ephemeralErr(http.StatusServiceUnavailable, errorCodeUpstream, "embed temporarily paused after Instagram blocked requests")

// enterPause lets a request through a direct path that *paused may hold shut:
// once the pause lapses, one probe per interval (the CAS winner) goes and
// everyone else falls through at once. probe is non-zero for that probe.
func enterPause(paused *atomic.Int64, interval time.Duration) (probe int64, ok bool) {
	now := time.Now().UnixNano()
	until := paused.Load()
	if until == 0 {
		return 0, true
	}
	if now < until || !paused.CompareAndSwap(until, now+int64(interval)) {
		return 0, false
	}
	return now + int64(interval), true
}

// pauseOn shuts the path after a 429 or login redirect and reports whether
// this started the pause.
func pauseOn(paused *atomic.Int64, interval time.Duration, err *AppError) bool {
	return (err.Status == http.StatusTooManyRequests || err.Code == errorCodeLoginRequired) &&
		paused.Swap(time.Now().Add(interval).UnixNano()) == 0
}

// resumeAfter reopens the path after a good probe. A response started before
// a newer block must not reopen it, hence the CAS on the probe's own deadline.
func resumeAfter(paused *atomic.Int64, probe int64) bool {
	return probe != 0 && paused.CompareAndSwap(probe, 0)
}

func (a *App) fetchPostEmbed(ctx context.Context, shortcode string) (Post, *AppError) {
	probe, ok := enterPause(&a.embedPausedUntil, embedProbeInterval)
	if !ok {
		return Post{}, errEmbedPaused
	}
	html, err := a.directGet(ctx, "post_embed", instagramOrigin+"/p/"+url.PathEscape(shortcode)+"/embed/captioned/")
	if err != nil {
		if pauseOn(&a.embedPausedUntil, embedProbeInterval, err) {
			logger(ctx).WarnContext(ctx, "post embed paused", "error_type", err.errorType(), "app_status", err.Status)
		}
		return Post{}, err
	}
	post, perr := parseEmbedPost(html)
	if perr != nil {
		logEmbedParseFailure(ctx, "post", html, perr)
	} else if resumeAfter(&a.embedPausedUntil, probe) {
		logger(ctx).InfoContext(ctx, "post embed resumed")
	}
	return post, perr
}

// fetchPostOG reads the Open Graph tags Instagram serves crawlers on the post
// page. Unlike every logged-out API they survive post-level gates.
func (a *App) fetchPostOG(ctx context.Context, shortcode string) (Post, *AppError) {
	probe, ok := enterPause(&a.ogPausedUntil, ogPageProbeInterval)
	if !ok {
		return Post{}, ephemeralErr(http.StatusServiceUnavailable, errorCodeUpstream, "og page paused after Instagram blocked requests")
	}
	ctx, cancel := context.WithTimeout(ctx, ogPageTimeout)
	defer cancel()
	// The tags sit in the head; a page past the body cap is cut, not refused.
	page, err := a.directFetch(ctx, fetchSpec{operation: "og_page", url: instagramOrigin + "/p/" + url.PathEscape(shortcode) + "/", cut: true})
	if err != nil {
		if pauseOn(&a.ogPausedUntil, ogPageProbeInterval, err) {
			logger(ctx).WarnContext(ctx, "og page paused", "error_type", err.errorType(), "app_status", err.Status)
		}
		return Post{}, err
	}
	// Any page counts as unblocked: account-gated posts never carry the tags.
	if resumeAfter(&a.ogPausedUntil, probe) {
		logger(ctx).InfoContext(ctx, "og page resumed")
	}
	post, perr := parseOGPost(shortcode, page)
	if perr != nil {
		logger(ctx).InfoContext(ctx, "og page had no card", "error", perr.logMessage(), "bytes", len(page))
	}
	return post, perr
}

var (
	ogMetaRE   = regexp.MustCompile(`<meta((?:\s+[\w:-]+="[^"]*")+)\s*/?>`)
	ogAttrRE   = regexp.MustCompile(`([\w:-]+)="([^"]*)"`)
	ogCountsRE = regexp.MustCompile(`^(?:([0-9][0-9.,]{0,12}[KMB]?) likes?(?:, | - ))?(?:([0-9][0-9.,]{0,12}[KMB]?) comments? - )?`)
)

// parseOGPost builds a partial post from the crawler tags: og:url names the
// author, og:description holds Instagram's rounded counts ("14K" stays text)
// and the caption, og:image a cover of unknown size (s640x640 only bounds it).
// Every value is untrusted page text; the titles open with the free-text
// display name, so the handle comes from og:url alone.
func parseOGPost(shortcode, page string) (Post, *AppError) {
	tags := map[string]string{}
	for _, m := range ogMetaRE.FindAllStringSubmatch(page, -1) {
		attrs := map[string]string{}
		for _, attr := range ogAttrRE.FindAllStringSubmatch(m[1], -1) {
			attrs[attr[1]] = attr[2]
		}
		key := cmp.Or(attrs["property"], attrs["name"])
		if content, ok := attrs["content"]; ok && key != "" && tags[key] == "" {
			tags[key] = html.UnescapeString(content)
		}
	}
	noCard := func(why string) (Post, *AppError) {
		return Post{}, igErr(http.StatusBadGateway, errorCodeUpstream, "og page "+why)
	}
	cover := tags["og:image"]
	image, err := url.Parse(cover)
	if err != nil || !trustedMediaURL(image) {
		return noCard("had no trusted og:image")
	}
	postURL, err := url.Parse(tags["og:url"])
	var path []string
	if err == nil && postURL.Scheme == "https" && postURL.Host == "www.instagram.com" {
		path = splitPath(postURL.Path)
	}
	if len(path) != 3 || path[2] != shortcode {
		return noCard("og:url did not name this post")
	}
	username := path[0]
	if !validUsername(username) {
		return noCard("named no author")
	}
	fullName := ""
	if i := strings.LastIndex(tags["twitter:title"], " (@"+username+")"); i >= 0 {
		fullName = tags["twitter:title"][:i]
	}
	// Either copy of the caption may be the cut one.
	caption := ogQuoted(tags["og:description"], `: "`)
	if title := ogQuoted(tags["og:title"], ` on Instagram: "`); len(title) > len(caption) {
		caption = title
	}
	var stats []string
	counts := ogCountsRE.FindStringSubmatch(tags["og:description"])
	if counts[1] != "" {
		stats = append(stats, "❤️ "+counts[1])
	}
	if counts[2] != "" {
		stats = append(stats, "\U0001f4ac "+counts[2])
	}
	return Post{
		Shortcode:   shortcode,
		Username:    username,
		FullName:    strings.TrimSpace(fullName),
		Caption:     normalizeCaption(caption),
		StatsLine:   strings.Join(stats, "  "),
		Attachments: []Attachment{{Kind: "image", URL: cover, Thumbnail: cover}},
		CreatedAt:   shortcodeTime(shortcode),
		Partial:     true,
	}, nil
}

// ogQuoted is the text between the first marker and the last double quote.
func ogQuoted(s, marker string) string {
	_, rest, ok := strings.Cut(s, marker)
	if i := strings.LastIndexByte(rest, '"'); ok && i >= 0 {
		return rest[:i]
	}
	return ""
}

func logEmbedParseFailure(ctx context.Context, operation, html string, err *AppError) {
	logger(ctx).WarnContext(ctx, "embed parse failed", "operation", operation, "error", err.logMessage(), "bytes", len(html),
		"context_json_null", strings.Contains(html, `"contextJSON":null`), "broken_media", strings.Contains(html, "EmbedBrokenMedia"))
}

func parseEmbedPost(page string) (Post, *AppError) {
	inner, err := embedContextJSON(page)
	if err != nil {
		return parseEmbedSimple(page)
	}
	sm := gjson.Get(inner, "gql_data.shortcode_media")
	if !present(sm) {
		return Post{}, igErr(502, errorCodeUpstream, "embed missing media")
	}
	return parseGraphMedia(sm)
}

var (
	simpleMediaTypeRE = regexp.MustCompile(`data-media-type="([^"]*)"`)
	simplePermalinkRE = regexp.MustCompile(`data-permalink="[^"]*?/p/([A-Za-z0-9_-]+)`)
	simpleUsernameRE  = regexp.MustCompile(`class="UsernameText">([^<]*)<`)
	simpleAvatarRE    = regexp.MustCompile(`(?s)class="Avatar[^"]*"[^>]*>\s*<img[^>]*\bsrc="([^"]*)"`)

	simpleCollabAvatarRE = regexp.MustCompile(`(?s)class="[^"]*SecondCollabAvatar"[^>]*>\s*<img[^>]*\bsrc="([^"]*)"`)
	simpleImageTagRE     = regexp.MustCompile(`(?s)<img class="EmbeddedMediaImage"[^>]*>`)
	simpleSrcRE          = regexp.MustCompile(`\bsrc="([^"]*)"`)
	simpleSrcsetRE       = regexp.MustCompile(`\bsrcset="([^"]*)"`)
	simpleFrameRatioRE   = regexp.MustCompile(`EmbedFrame"[^>]*padding-bottom:\s*([\d.]+)%`)
	simpleLikesRE        = regexp.MustCompile(`>([\d,]+)\s+likes?<`)
	simpleCaptionUserRE  = regexp.MustCompile(`(?s)^\s*<a class="CaptionUsername"[^>]*>[^<]*</a>`)
	simpleBrRE           = regexp.MustCompile(`(?i)<br\s*/?>`)
	simpleTagRE          = regexp.MustCompile(`<[^>]+>`)
)

func parseEmbedSimple(page string) (Post, *AppError) {
	mediaType := firstGroup(simpleMediaTypeRE, page)
	if mediaType == "" {
		return Post{}, igErr(502, errorCodeUpstream, "simple embed: no media node")
	}
	if !strings.Contains(mediaType, "Image") {
		return Post{}, igErr(502, errorCodeUpstream, "simple embed: unsupported media type "+mediaType)
	}
	username := firstGroup(simpleUsernameRE, page)
	if username == "" {
		return Post{}, igErr(502, errorCodeUpstream, "simple embed missing owner")
	}

	imgTag := simpleImageTagRE.FindString(page)
	imgURL, w := bestSrcset(firstGroup(simpleSrcsetRE, imgTag))
	if imgURL == "" {
		imgURL = html.UnescapeString(firstGroup(simpleSrcRE, imgTag))
	}
	if imgURL == "" {
		return Post{}, igErr(502, errorCodeUpstream, "simple embed had no image")
	}

	h := 0
	if ratio, err := strconv.ParseFloat(firstGroup(simpleFrameRatioRE, page), 64); err == nil && w > 0 {
		h = int(float64(w) * ratio / 100)
	}

	att := Attachment{
		Kind:      "image",
		URL:       imgURL,
		Thumbnail: imgURL,
		Width:     w,
		Height:    h,
	}

	return Post{
		Shortcode:   firstGroup(simplePermalinkRE, page),
		Username:    username,
		FullName:    "",
		ProfilePic:  html.UnescapeString(cmp.Or(firstGroup(simpleAvatarRE, page), firstGroup(simpleCollabAvatarRE, page))),
		Caption:     simpleCaption(page),
		StatsLine:   statsLine("", parseCount(firstGroup(simpleLikesRE, page)), simpleCommentCount(page)),
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
	n, err := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	if err != nil {
		return -1
	}
	return n
}

// Find the literal first; digits and whitespace are matched as ASCII only.
// The caption comes earlier in the page and may itself contain "N comments".
func simpleCommentCount(page string) int {
	_, page, ok := strings.Cut(page, `<div class="CaptionComments"`)
	if !ok {
		return -1
	}
	for {
		i := strings.Index(page, "comment")
		if i < 0 {
			return -1
		}
		end := i
		for end > 0 && strings.ContainsRune(" \t\r\n\f", rune(page[end-1])) {
			end--
		}
		start := end
		for start > 0 && (page[start-1] == ',' || (page[start-1] >= '0' && page[start-1] <= '9')) {
			start--
		}
		if start < end && end < i {
			return parseCount(page[start:end])
		}
		page = page[i+len("comment"):]
	}
}

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
	_, rest, ok := strings.Cut(page, `<div class="Caption">`)
	if !ok {
		return ""
	}
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

func parseGraphMedia(sm gjson.Result) (Post, *AppError) {
	owner := sm.Get("owner")
	username := owner.Get("username").String()
	if username == "" {
		return Post{}, igErr(502, errorCodeUpstream, "embed missing owner")
	}

	var atts []Attachment
	var err *AppError
	add := func(n gjson.Result) bool {
		var att Attachment
		att, err = graphAttachment(n)
		atts = append(atts, att)
		return err == nil
	}
	if kids := sm.Get("edge_sidecar_to_children.edges"); len(kids.Array()) > 0 {
		kids.ForEach(func(_, e gjson.Result) bool { return add(e.Get("node")) })
	} else {
		add(sm)
	}
	if err != nil {
		return Post{}, err
	}

	return Post{
		Shortcode:   sm.Get("shortcode").String(),
		Username:    username,
		FullName:    owner.Get("full_name").String(),
		ProfilePic:  owner.Get("profile_pic_url").String(),
		IsVerified:  owner.Get("is_verified").Type == gjson.True,
		Caption:     sm.Get("edge_media_to_caption.edges.0.node.text").String(),
		StatsLine:   statsLine(v1StatsPrefix(sm), countOf(sm, "edge_liked_by.count"), countOf(sm, "edge_media_to_comment.count")),
		Attachments: atts,
	}, nil
}

func graphAttachment(n gjson.Result) (Attachment, *AppError) {
	img := bestGraphImageURL(n)
	if img == "" {
		return Attachment{}, igErr(502, errorCodeUpstream, "embed media had no image")
	}
	w, h := uintOf(n, "dimensions.width"), uintOf(n, "dimensions.height")
	if n.Get("is_video").Bool() {
		u := n.Get("video_url").String()
		if u == "" {
			return Attachment{}, igErr(502, errorCodeUpstream, "embed video blocked")
		}
		return Attachment{Kind: "video", URL: u, Thumbnail: img, Width: w, Height: h}, nil
	}
	return Attachment{Kind: "image", URL: img, Thumbnail: img, Width: w, Height: h}, nil
}

func bestGraphImageURL(n gjson.Result) string {
	return cmp.Or(n.Get("display_url").String(), bestCandidateURL(n.Get("display_resources")))
}

func (a *App) fetchProfileEmbed(ctx context.Context, username string) (Profile, *AppError) {
	html, err := a.directGet(ctx, "profile_embed", instagramOrigin+"/"+url.PathEscape(username)+"/embed/")
	if err != nil {
		return Profile{}, err
	}
	profile, perr := parseEmbedProfile(html)
	if perr != nil {
		logEmbedParseFailure(ctx, "profile", html, perr)
	}
	return profile, perr
}

func parseEmbedProfile(html string) (Profile, *AppError) {
	inner, err := embedContextJSON(html)
	if err != nil {
		return Profile{}, err
	}
	ctx := gjson.Get(inner, "context")
	username := ctx.Get("username").String()
	if username == "" {
		return Profile{}, igErr(502, errorCodeUpstream, "embed profile missing username")
	}
	p := Profile{
		Username:      username,
		FullName:      ctx.Get("full_name").String(),
		ProfilePic:    ctx.Get("profile_pic_url").String(),
		FollowerCount: uintOf(ctx, "followers_count"),
		MediaCount:    uintOf(ctx, "posts_count"),
		IsPrivate:     ctx.Get("is_private").Bool(),
		IsVerified:    ctx.Get("is_verified").Type == gjson.True,
	}
	ctx.Get("graphql_media").ForEach(func(_, m gjson.Result) bool {
		sm := m.Get("shortcode_media")
		thumb := bestGraphImageURL(sm)
		if thumb != "" {
			p.RecentMedia = append(p.RecentMedia, ProfileMedia{
				Thumbnail: thumb,
				Width:     uintOf(sm, "dimensions.width"),
				Height:    uintOf(sm, "dimensions.height"),
			})
		}
		return len(p.RecentMedia) < profileGalleryMax
	})
	return p, nil
}
