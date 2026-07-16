package main

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

var storyIDRE = regexp.MustCompile(`^[0-9]{1,32}$`)

func validStoryID(id string) bool { return storyIDRE.MatchString(id) }

func (a *App) getStory(ctx context.Context, username, id string, meta *fetchMeta) (Story, *AppError) {
	if !validStoryID(id) {
		return Story{}, igErr(404, reasonNotFound, "invalid story id")
	}
	return a.stories.get(ctx, username+"/"+id, meta, func() (Story, time.Duration, *AppError) {
		if externalHelperStoryFn == nil {
			return Story{}, 0, igErr(404, reasonMediaNotFound, "story fetching is not available")
		}
		story, err := externalHelperStoryFn(a, ctx, username, id)
		if err != nil {
			return Story{}, 0, err
		}
		return story, cacheTTLFromURLs(story.Media.URL, story.Media.Thumbnail), nil
	})
}

func storyOriginURL(username, id string) string {
	return instagramOrigin + "/stories/" + url.PathEscape(username) + "/" + url.PathEscape(id) + "/"
}

func storyOffloadURL(baseURL, username, id string, thumbnail bool) string {
	suffix := ""
	if thumbnail {
		suffix = "?thumbnail=1"
	}
	return baseURL + "/offload/story/" + url.PathEscape(username) + "/" + url.PathEscape(id) + suffix
}

func storyAvatarURL(baseURL string, story Story) string {
	if story.ProfilePic == "" {
		return ""
	}
	return baseURL + "/offload/story/" + url.PathEscape(story.Username) + "/" + url.PathEscape(story.ID) + "/avatar"
}

func (a *App) handleStory(req *http.Request, username, id string) resp {
	origin := storyOriginURL(username, id)
	baseURL := a.publicBaseURL(req)
	gallery := galleryRequested(req.URL.Query())
	meta := &fetchMeta{}
	story, err := a.getStory(req.Context(), username, id, meta)
	if err != nil {
		title, desc := postErrorCard(err.Reason, supportURL)
		r := htmlResp(200, a.buildStatusEmbedHTML(baseURL, origin, title, desc))
		r.headers["og-status"] = strconv.Itoa(err.Status)
		if err.Reason != "" {
			r.headers["og-reason"] = err.Reason
		}
		return tagFetch(cacheable(r, errorCacheSeconds(err.Reason)), meta)
	}
	html := a.buildStoryEmbedHTML(baseURL, req.Header.Get("User-Agent"), origin, story,
		storyOffloadURL(baseURL, username, id, false), storyOffloadURL(baseURL, username, id, true),
		storyStatusURL(baseURL, username, id, gallery), gallery)
	return tagFetch(cacheable(htmlResp(200, html), edgeCacheSeconds), meta)
}

func (a *App) handleStoryOffload(req *http.Request, username, id string, avatar bool) resp {
	if !allowOffloadFetch(req) && !a.stories.known(req.Context(), username+"/"+id) {
		return resp{status: 404, headers: map[string]string{}}
	}
	meta := &fetchMeta{}
	story, err := a.getStory(req.Context(), username, id, meta)
	if err != nil {
		return tagFetch(offloadErrorResp(err, storyOriginURL(username, id)), meta)
	}
	target := story.Media.URL
	switch {
	case avatar:
		target = story.ProfilePic
	case req.URL.Query().Has("thumbnail") && story.Media.Thumbnail != "":
		target = story.Media.Thumbnail
	}
	if target == "" {
		target = a.publicBaseURL(req) + defaultAvatarPath
	}
	return tagFetch(cacheable(redirectResp(target, 302), cdnEdgeSeconds(target)), meta)
}

func (a *App) buildStoryEmbedHTML(baseURL, ua, origin string, story Story, mediaHref, thumbnailHref, activityHref string, gallery bool) string {
	media := story.Media
	title := displayTitle(story.FullName, story.Username)
	description := postDescription(story.Caption, "")
	if gallery {
		description = ""
	}

	h := a.commonHead(baseURL, origin, story.Username, title, description, thumbnailHref, "summary_large_image", activityHref)
	h = append(h,
		`<meta property="og:type" content="article">`,
		`<meta property="article:author" content="`+instagramOrigin+"/"+html.EscapeString(story.Username)+`/">`,
	)
	h = append(h, dimensionTags("og:image", media.Width, media.Height)...)
	if avatar := storyAvatarURL(baseURL, story); avatar != "" {
		h = append(h, `<link rel="apple-touch-icon" href="`+html.EscapeString(avatar)+`">`)
	}
	if published := isoTime(story.CreatedAt); published != "" {
		h = append(h, `<meta property="article:published_time" content="`+html.EscapeString(published)+`">`)
	}
	if !isTelegramBot(ua) {
		h = append(h, `<meta http-equiv="refresh" content="0;url=`+html.EscapeString(origin)+`">`)
	}
	if media.Kind == "video" {
		h = append(h, videoOGTags(mediaHref, media)...)
	}
	return embedDocument(h)
}
