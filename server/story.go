package main

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var storyIDRE = regexp.MustCompile(`^[0-9]{1,32}$`)

func validStoryID(id string) bool { return storyIDRE.MatchString(id) }

func (a *App) getStory(ctx context.Context, username, id string, meta *fetchMeta) (Story, *AppError) {
	if !validUsername(username) || !validStoryID(id) {
		return Story{}, igErr(404, errorCodeNotFound, "invalid story")
	}
	username = strings.ToLower(username)
	return a.stories.get(ctx, username+"/"+id, meta, func(fetchCtx context.Context) (Story, time.Duration, bool, *AppError) {
		story, err := externalHelperStoryImpl(a, fetchCtx, username, id)
		return story, cacheTTLFromURLs(story.ProfilePic, story.Media.URL, story.Media.Thumbnail), true, err
	})
}

func storyOriginURL(username, id string) string {
	return instagramOrigin + "/stories/" + url.PathEscape(username) + "/" + url.PathEscape(id) + "/"
}

func (a *App) storyOffloadURL(baseURL, username, id string, thumbnail bool) string {
	path := "/offload/story/" + url.PathEscape(strings.ToLower(username)) + "/" + url.PathEscape(id)
	return a.offloadSigner.url(baseURL, path, thumbnail)
}

func (a *App) storyAvatarURL(baseURL string, story Story) string {
	if story.ProfilePic == "" {
		return ""
	}
	path := "/offload/story/" + url.PathEscape(strings.ToLower(story.Username)) + "/" + url.PathEscape(story.ID) + "/avatar"
	return a.offloadSigner.url(baseURL, path, false)
}

func (a *App) handleStory(req *http.Request, username, id string, gallery bool) resp {
	origin := storyOriginURL(username, id)
	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	story, err := a.getStory(req.Context(), username, id, meta)
	if err != nil {
		title, desc := errorCard("story", err.Code)
		return a.errorCardResp(baseURL, origin, title, desc, err.Code, err, meta)
	}
	variant := discordRequestVariant(req)
	html := a.buildStoryEmbedHTML(baseURL, origin, story,
		a.storyOffloadURL(baseURL, username, id, false), a.storyOffloadURL(baseURL, username, id, true),
		storyStatusURL(baseURL, username, id, gallery), gallery, variant)
	return tagFetch(discordEmbedResponse(req, html, variant), meta)
}

func (a *App) handleStoryOffload(req *http.Request, username, id string, avatar bool) resp {
	username = strings.ToLower(username)
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
	return tagFetch(redirectResp(target, 302), meta)
}

func (a *App) buildStoryEmbedHTML(baseURL, origin string, story Story, mediaHref, thumbnailHref, activityHref string, gallery bool, variant discordEmbedVariant) string {
	media := story.Media
	title := displayTitle(story.FullName, story.Username)
	description := postDescription(story.Caption, "")
	if gallery {
		description = ""
	}
	component := ""
	if variant != discordLegacy {
		component = a.storyComponentEmbed(baseURL, story, gallery, variant == discordComponentsSupport)
	}
	if component != "" {
		activityHref = ""
	}

	h := a.commonHead(baseURL, origin, story.Username, title, description, thumbnailHref, "summary_large_image", activityHref)
	h = append(h,
		`<meta property="og:type" content="article">`,
		`<meta property="article:author" content="`+instagramOrigin+"/"+html.EscapeString(story.Username)+`/">`,
	)
	h = append(h, dimensionTags("property", "og:image", media.Width, media.Height)...)
	if avatar := a.storyAvatarURL(baseURL, story); avatar != "" {
		h = append(h, `<link rel="apple-touch-icon" href="`+html.EscapeString(avatar)+`">`)
	}
	if published := isoTime(story.CreatedAt); published != "" {
		h = append(h, `<meta property="article:published_time" content="`+html.EscapeString(published)+`">`)
	}
	if media.Kind == "video" {
		h = append(h, videoOGTags(mediaHref, media)...)
	}
	if component != "" {
		h = append(h, component)
	}
	return embedDocument(h)
}
