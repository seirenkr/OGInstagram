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
	"time"

	"github.com/tidwall/gjson"
)

func (a *App) directGet(ctx context.Context, operation, rawURL string) (out string, ferr *AppError) {
	started := time.Now()
	status := 0
	defer func() {
		logOutbound(ctx, operation, "direct", http.MethodGet, rawURL, started, status, len(out), ferr, false)
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
	status, out, ferr = fetchRequest(ctx, &client, fetchSpec{
		operation: operation,
		method:    http.MethodGet,
		url:       rawURL,
		subject:   "Instagram",
		headers:   map[string]string{"User-Agent": embedUA},
		interpret: statusOnly,
	})
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

func (a *App) fetchPostEmbed(ctx context.Context, shortcode string) (Post, *AppError) {
	// While Instagram blocks the embed, skip it and let one probe through per
	// embedProbeInterval (the CAS winner); everyone else falls through at once.
	now := time.Now().UnixNano()
	until := a.embedPausedUntil.Load()
	if until != 0 {
		if now < until || !a.embedPausedUntil.CompareAndSwap(until, now+int64(embedProbeInterval)) {
			return Post{}, errEmbedPaused
		}
		until = now + int64(embedProbeInterval)
	}
	html, err := a.directGet(ctx, "post_embed", instagramOrigin+"/p/"+url.PathEscape(shortcode)+"/embed/captioned/")
	if err != nil {
		if err.Status == http.StatusTooManyRequests || err.Code == errorCodeLoginRequired {
			if a.embedPausedUntil.Swap(time.Now().Add(embedProbeInterval).UnixNano()) == 0 {
				logger(ctx).WarnContext(ctx, "post embed paused", "error_type", err.errorType(), "app_status", err.Status)
			}
		}
		return Post{}, err
	}
	post, perr := parseEmbedPost(html)
	if perr != nil {
		logEmbedParseFailure(ctx, "post", html, perr)
	} else if until != 0 && a.embedPausedUntil.CompareAndSwap(until, 0) {
		// A response started before a newer block must not reopen the embed path.
		logger(ctx).InfoContext(ctx, "post embed resumed")
	}
	return post, perr
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
	simpleOwnerIDRE   = regexp.MustCompile(`data-owner-id="([^"]*)"`)
	simpleMediaIDRE   = regexp.MustCompile(`data-media-id="([^"]*)"`)
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
		ID:        firstGroup(simpleMediaIDRE, page),
		Kind:      "image",
		URL:       imgURL,
		Thumbnail: imgURL,
		Width:     w,
		Height:    h,
	}

	return Post{
		Shortcode:   firstGroup(simplePermalinkRE, page),
		Username:    username,
		OwnerID:     firstGroup(simpleOwnerIDRE, page),
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
		OwnerID:     owner.Get("id").String(),
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
	id := cmp.Or(n.Get("id").String(), n.Get("pk").String())
	if n.Get("is_video").Bool() {
		u := n.Get("video_url").String()
		if u == "" {
			return Attachment{}, igErr(502, errorCodeUpstream, "embed video blocked")
		}
		return Attachment{ID: id, Kind: "video", URL: u, Thumbnail: img, Width: w, Height: h}, nil
	}
	return Attachment{ID: id, Kind: "image", URL: img, Thumbnail: img, Width: w, Height: h}, nil
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
		UserID:        ctx.Get("owner_id").String(),
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
				ID:        cmp.Or(sm.Get("id").String(), sm.Get("shortcode").String()),
				Thumbnail: thumb,
				Width:     uintOf(sm, "dimensions.width"),
				Height:    uintOf(sm, "dimensions.height"),
				TakenAt:   unixTime(sm.Get("taken_at_timestamp").Int()),
			})
		}
		return len(p.RecentMedia) < profileGalleryMax
	})
	return p, nil
}
