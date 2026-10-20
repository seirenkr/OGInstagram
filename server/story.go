package main

import (
	"context"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

var storyIDRE = regexp.MustCompile(`^[0-9]{1,32}$`)

func validStoryID(id string) bool { return storyIDRE.MatchString(id) }

func (a *App) getStory(ctx context.Context, username, id string, meta *fetchMeta) (Story, *AppError) {
	if !validUsername(username) || !validStoryID(id) {
		return Story{}, igErr(404, errorCodeNotFound, "invalid story")
	}
	username = strings.ToLower(username)
	key := username + "/" + id
	valid := validSourceModel[Story](key)
	return a.stories.get(ctx, key, meta, func(fetchCtx context.Context) (Story, time.Duration, bool, *AppError) {
		var sources []stagedSource[Story]
		if pk, err := strconv.ParseUint(id, 10, 64); err == nil {
			sources = append(sources, stagedSource[Story]{name: "story_graphql", persist: true, fetch: func(ctx context.Context) (Story, *AppError) {
				spec := webLoggedOutSpec(pkShortcode(pk))
				spec.operation = "story_graphql"
				_, body, err := a.fetchViaProxy(ctx, spec)
				if err != nil {
					return Story{}, err
				}
				story, perr := parseGraphQLStory(username, id, body)
				if live := time.Since(time.UnixMilli(int64(pk>>23)+igEpochMs)) < 24*time.Hour; live {
					// Final unless the path looks closed: the helper serves deleted stories from cache.
					if a.storyDoor.closed(perr != nil && perr.Code == errorCodeGraphQL) {
						return Story{}, perr
					}
				}
				if perr != nil && perr.Code == errorCodeGraphQL && gjson.Get(body, "errors.0.code").Int() == graphQLFieldExceptionCode {
					notFound := igErr(http.StatusNotFound, errorCodeMediaNotFound, "story not available")
					notFound.Final = true
					return Story{}, notFound
				}
				return valid(story, perr)
			}})
		}
		sources = append(sources, stagedSource[Story]{name: "story_helper", after: storyHelperHedgeDelay, persist: true, fetch: func(ctx context.Context) (Story, *AppError) {
			return valid(externalHelperStoryImpl(a, ctx, username, id))
		}})
		if len(sources) == 1 {
			sources[0].after = 0
		}
		story, persist, err := stagedFetch(fetchCtx, sources...)
		return story, cacheTTLFromURLs(story.ProfilePic, story.Media.URL, story.Media.Thumbnail), persist, err
	})
}

// parseGraphQLStory checks owner, id and kind: the post query ignores the URL's username.
func parseGraphQLStory(username, id, body string) (Story, *AppError) {
	post, err := parseInstagramPost(body)
	if err != nil {
		return Story{}, err
	}
	item := gjson.Get(body, "data.xdt_api__v1__media__shortcode__web_info.items.0")
	if item.Get("product_type").String() != "story" || item.Get("pk").String() != id || !strings.EqualFold(post.Username, username) {
		notFound := igErr(http.StatusNotFound, errorCodeMediaNotFound, "story not available")
		notFound.Final = true
		return Story{}, notFound
	}
	return Story{
		ID:         id,
		Username:   post.Username,
		FullName:   post.FullName,
		ProfilePic: post.ProfilePic,
		IsVerified: post.IsVerified,
		Media:      post.Attachments[0],
		CreatedAt:  post.CreatedAt,
	}, nil
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
		return baseURL + defaultAvatarPath
	}
	path := "/offload/story/" + url.PathEscape(strings.ToLower(story.Username)) + "/" + url.PathEscape(story.ID) + "/avatar"
	return a.offloadSigner.url(baseURL, path, false)
}

func (a *App) handleStory(req *http.Request, username, id string, gallery bool) resp {
	origin := storyOriginURL(username, id)
	baseURL := a.publicBaseURL(req)
	meta := &fetchMeta{}
	story, err := a.getStory(req.Context(), username, id, meta)
	activity := wantsActivity(req)
	if err != nil {
		if activity {
			return tagFetch(activityErrorResp(err), meta)
		}
		title, desc := errorCard("story", err.Code)
		return a.errorCardResp(baseURL, origin, title, desc, err.Code, err, meta)
	}
	objectID, actor := storyRefs(baseURL, story)
	if activity {
		return tagFetch(activityResp(a.storyNote(baseURL, story, gallery), objectID, actor), meta)
	}
	html := a.buildStoryEmbedHTML(baseURL, origin, story,
		a.storyOffloadURL(baseURL, username, id, false), a.storyOffloadURL(baseURL, username, id, true), gallery, discordSupport(req))
	return tagFetch(cardResp(html, objectID, actor), meta)
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
	case req.URL.Query().Has("thumbnail"):
		target = story.Media.Thumbnail
	}
	return tagFetch(a.offloadResp(req, target, avatar), meta)
}

func (a *App) buildStoryEmbedHTML(baseURL, origin string, story Story, mediaHref, thumbnailHref string, gallery, support bool) string {
	media := story.Media
	title := displayTitle(story.FullName, story.Username)
	description := postDescription(story.Caption, "")
	if gallery {
		description = ""
	}

	h := a.commonHead(baseURL, origin, story.Username, title, description, thumbnailHref, "summary_large_image")
	h = append(h,
		`<meta property="og:type" content="article">`,
		`<meta property="article:author" content="`+instagramOrigin+"/"+html.EscapeString(story.Username)+`/">`,
	)
	h = append(h, dimensionTags("og:image", media.Width, media.Height)...)
	h = append(h, `<link rel="apple-touch-icon" href="`+html.EscapeString(a.storyAvatarURL(baseURL, story))+`">`)
	if published := isoTime(story.CreatedAt); published != "" {
		h = append(h, `<meta property="article:published_time" content="`+published+`">`, `<meta property="og:pubdate" content="`+published+`">`)
	}
	if media.Kind == "video" {
		h = append(h, videoOGTags(mediaHref, media)...)
	}
	h = append(h, apLinkTags(storyRefs(baseURL, story))...)
	h = append(h, a.storyComponentEmbed(baseURL, story, gallery, support))
	return embedDocument(h)
}

// storySideDoor watches 1675030 among live stories (normally ~8%); far more
// means the post query no longer serves stories, and the helper takes over.
type storySideDoor struct {
	mu       sync.Mutex
	ring     [100]bool // true: 1675030
	n, i     int
	hidden   int
	loggedAt time.Time
}

const storySideDoorClosedPercent = 40

func (d *storySideDoor) closed(hidden bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n == len(d.ring) {
		if d.ring[d.i] {
			d.hidden--
		}
	} else {
		d.n++
	}
	d.ring[d.i] = hidden
	if hidden {
		d.hidden++
	}
	d.i = (d.i + 1) % len(d.ring)
	closed := d.n >= len(d.ring)/2 && d.hidden*100 > d.n*storySideDoorClosedPercent
	if closed && time.Since(d.loggedAt) > time.Minute {
		d.loggedAt = time.Now()
		slog.Warn("story graphql looks closed; the external helper answers 1675030 stories", "hidden", d.hidden, "of", d.n)
	}
	return closed && hidden
}
