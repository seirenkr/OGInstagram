package main

import (
	"context"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

type App struct {
	cfg           Config
	pool          *SessionPool
	offloadSigner offloadSigner
	helper        externalHelperClient

	posts    *cache[Post]
	profiles *cache[Profile]
	stories  *cache[Story]
	shares   *cache[shareTarget]
	ogPosts  *cache[Post]

	fetchSlots chan struct{}

	v6, v6Pages *SessionPool
	v6Gates     sync.Map
	helperSlots chan struct{}
	storyDoor   storySideDoor

	gateNotices sync.Map
}

func newApp(cfg Config, pool *SessionPool, signer offloadSigner) *App {
	fetchSlots := make(chan struct{}, maxConcurrentFetches)
	a := &App{
		cfg:           cfg,
		pool:          pool,
		offloadSigner: signer,
		fetchSlots:    fetchSlots,
		posts:         newPersistentCache[Post](cfg.Store, "post", fetchSlots, localPostCacheBytes),
		profiles:      newPersistentCache[Profile](cfg.Store, "profile", fetchSlots, localProfileCacheBytes),
		stories:       newPersistentCache[Story](cfg.Store, "story", fetchSlots, localStoryCacheBytes),
		shares:        newPersistentCache[shareTarget](cfg.Store, "share", fetchSlots, localShareCacheBytes),
		ogPosts:       newPersistentCache[Post](nil, "og_post", fetchSlots, localOGPostCacheBytes),
	}
	a.helperSlots = make(chan struct{}, helperBackgroundSlots)
	if cfg.EgressSocket != "" {
		a.v6, a.v6Pages = newEgressPool(cfg.EgressSocket, "v6-"), newEgressPool(cfg.EgressSocket, "v6p-")
	}
	if newExternalHelperClient != nil {
		a.helper = newExternalHelperClient(pool)
	}
	return a
}

type fetchMeta struct{ fetched bool }

func (a *App) getPost(ctx context.Context, shortcode string, meta *fetchMeta) (Post, *AppError) {
	if !validPostShortcode(shortcode) {
		return Post{}, igErr(404, errorCodeNotFound, "invalid shortcode")
	}
	post, err := a.posts.get(ctx, shortcode, meta, func(fetchCtx context.Context) (Post, time.Duration, bool, *AppError) {
		return a.fetchPost(fetchCtx, shortcode)
	})
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		if e, ok := a.ogPosts.localGet(shortcode); ok {
			logger(ctx).InfoContext(ctx, "partial post served at deadline")
			return e.value, nil
		}
		if v, ok := a.gateNotices.Load(shortcode); ok {
			notice := *v.(*AppError)
			return Post{}, &notice
		}
	}
	return post, err
}

func postCacheTTL(post Post) time.Duration {
	urls := []string{post.ProfilePic}
	for _, att := range post.Attachments {
		urls = append(urls, att.URL, att.Thumbnail)
	}
	return cacheTTLFromURLs(urls...)
}

func helperAsked(err *AppError) bool {
	return err != nil && !err.Ephemeral && err.Code != errorCodeConnection && err.Code != errorCodeRateLimited
}

type postHelper struct {
	once   sync.Once
	begun  atomic.Bool
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	post   Post
	err    *AppError
	run    func(context.Context) (Post, *AppError)
}

func (a *App) newPostHelper(ctx context.Context, shortcode string) *postHelper {
	h := &postHelper{done: make(chan struct{})}
	h.ctx, h.cancel = context.WithCancel(context.WithoutCancel(ctx))
	valid := validSourceModel[Post](shortcode)
	h.run = func(ctx context.Context) (Post, *AppError) {
		if externalHelperPostImpl == nil {
			return Post{}, ephemeralErr(http.StatusNotImplemented, errorCodeUpstream, "external helper is not available")
		}
		return valid(externalHelperPostImpl(a, ctx, shortcode))
	}
	return h
}

func (h *postHelper) start() {
	h.once.Do(func() {
		h.begun.Store(true)
		go func() {
			defer close(h.done)
			defer func() {
				if r := recover(); r != nil {
					logger(h.ctx).ErrorContext(h.ctx, "external helper panicked", "panic", r, "stack", string(debug.Stack()))
					h.err = ephemeralErr(http.StatusBadGateway, errorCodeUpstream, "external helper panicked")
				}
			}()
			workCtx, cancel := context.WithTimeout(h.ctx, helperDetachedTimeout)
			defer cancel()
			h.post, h.err = h.run(workCtx)
		}()
	})
}

func (h *postHelper) wait(ctx context.Context) (Post, *AppError) {
	h.start()
	select {
	case <-h.done:
		return h.post, h.err
	case <-ctx.Done():
		return Post{}, contextAppError(ctx)
	}
}

func (h *postHelper) asked() bool {
	select {
	case <-h.done:
		return helperAsked(h.err)
	default:
		return false
	}
}

func (a *App) keepAfterFlight(h *postHelper, shortcode string) {
	if !h.begun.Load() {
		h.cancel()
		return
	}
	select {
	case <-h.done:
		h.cancel()
		return
	case a.helperSlots <- struct{}{}:
	default:
		h.cancel()
		return
	}
	go func() {
		defer func() { <-a.helperSlots }()
		defer h.cancel()
		<-h.done
		if h.err == nil {
			a.storeHelperPost(h.ctx, shortcode, h.post)
		}
	}()
}

func (a *App) fetchPost(ctx context.Context, shortcode string) (post Post, ttl time.Duration, persist bool, err *AppError) {
	valid := validSourceModel[Post](shortcode)
	helper := a.newPostHelper(ctx, shortcode)
	var hidden atomic.Pointer[graphQLGating]
	defer func() {
		if hidden.Load() != nil && (err != nil || post.Partial) {
			a.keepAfterFlight(helper, shortcode)
		} else {
			helper.cancel()
		}
		if err == nil && post.CreatedAt.IsZero() {
			post.CreatedAt = shortcodeTime(shortcode)
		}
	}()

	sources := []stagedSource[Post]{
		{name: "post_graphql", fetch: func(ctx context.Context) (Post, *AppError) {
			_, body, gqlErr := a.fetchViaProxy(ctx, webLoggedOutSpec(shortcode))
			if gqlErr != nil {
				return Post{}, gqlErr
			}
			post, perr := parseInstagramPost(body)
			if perr != nil && graphQLHidden(body) {
				g := parseGraphQLGating(body)
				hidden.Store(&g)
				perr.Final = true
			}
			return valid(post, perr)
		}},
		{name: "post_embed", after: postHedgeDelay, fetch: func(ctx context.Context) (Post, *AppError) {
			return valid(a.fetchPostEmbed(ctx, shortcode))
		}},
	}
	if externalHelperPostImpl != nil {
		sources = append(sources, stagedSource[Post]{name: "post_helper", after: externalHelperHedgeDelay, fetch: helper.wait})
	}
	post, err = stagedFetch(ctx, sources...)
	if err == nil {
		return post, postCacheTTL(post), true, nil
	}
	if g := hidden.Load(); g != nil {
		return a.hiddenPost(ctx, shortcode, helper, *g)
	}
	outcome := a.fetchOembed(ctx, shortcode)
	card, cardErr := oembedVerdict(outcome, shortcode, *err)
	if card.Shortcode != "" {
		return card, min(postCacheTTL(card), transientErrorCacheSeconds*time.Second), false, nil
	}
	return Post{}, 0, false, gateOrRetry(cardErr, helper)
}

func (a *App) hiddenPost(ctx context.Context, shortcode string, helper *postHelper, gating graphQLGating) (Post, time.Duration, bool, *AppError) {
	helper.start()
	oembedC := make(chan oembedOutcome, 1)
	ogC, dcC, missingC := make(chan Post, 1), make(chan Post, 1), make(chan bool, 1)
	pending := 0
	follow := func(f func()) {
		pending++
		go f()
	}
	retryDC := func() {
		if a.v6 == nil {
			return
		}
		follow(func() {
			var post Post
			if _, body, err := a.fetchDC(ctx, webLoggedOutSpec(shortcode), dcAttemptTimeout, dcAttemptTimeout); err == nil {
				if p, perr := validSourceModel[Post](shortcode)(parseInstagramPost(body)); perr == nil {
					post = p
				}
			}
			dcC <- post
		})
	}
	fetchOG := func() {
		follow(func() {
			og, err := validSourceModel[Post](shortcode)(a.fetchPostOG(ctx, shortcode))
			if err != nil {
				og = Post{}
			}
			ogC <- og
		})
	}
	followGate := func(gatingType int) {
		switch gatingType {
		case 3, 10, 11:
			fetchOG()
		case 2:
			fetchOG()
			retryDC()
		}
	}

	base := *igErr(http.StatusBadGateway, errorCodeGraphQL, "Instagram could not load this post")
	var partial Post
	var gate *AppError
	oembedStatus := 0
	oembedGone := false
	if gating.gatingType != 0 && gating.title != "" {
		gate = gatingError(gating.gatingType, gating.title, gating.desc, base)
		a.publishGate(shortcode, gate)
		followGate(gating.gatingType)
	} else {
		follow(func() { oembedC <- a.fetchOembed(ctx, shortcode) })
	}

	answer := func(helperErr *AppError) (Post, time.Duration, bool, *AppError) {
		switch {
		case partial.Shortcode != "":
			ttl := min(postCacheTTL(partial), permanentErrorCacheSeconds*time.Second)
			if partial.Partial && !helperAsked(helperErr) || oembedStatus == http.StatusOK {
				ttl = min(ttl, transientErrorCacheSeconds*time.Second)
			}
			return partial, ttl, false, nil
		case gate != nil:
			return Post{}, 0, false, gateOrRetry(gate, helper)
		case oembedGone && helperErr != nil && helperErr.Status == http.StatusNotFound:
			return Post{}, 0, false, igErr(http.StatusNotFound, errorCodeMediaNotFound, "This post isn't available.")
		}
		return Post{}, 0, false, &base
	}
	helperDone := helper.done
	var helperErr *AppError
	for {
		if helperErr != nil && pending == 0 {
			return answer(helperErr)
		}
		select {
		case <-helperDone:
			if helper.err == nil {
				return helper.post, postCacheTTL(helper.post), true, nil
			}
			helperErr, helperDone = helper.err, nil
		case o := <-oembedC:
			pending--
			oembedStatus = o.status
			card, cardErr := oembedVerdict(o, shortcode, base)
			switch {
			case card.Shortcode != "":
				partial = card
				a.ogPosts.put(ctx, shortcode, card, min(postCacheTTL(card), partialPostTTL))
				retryDC()
			case cardErr.CardTitle != "":
				gate = cardErr
				a.publishGate(shortcode, cardErr)
				followGate(int(gjson.Get(o.body, "gating_type_thrift").Int()))
			case oembedNotFound(o):
				oembedGone = true
				follow(func() {
					missingC <- a.mediaMissing(ctx, shortcode)
				})
			}
		case og := <-ogC:
			pending--
			if og.Shortcode != "" {
				partial = og
				a.ogPosts.put(ctx, shortcode, og, min(postCacheTTL(og), partialPostTTL))
			}
		case post := <-dcC:
			pending--
			if post.Shortcode != "" {
				return post, postCacheTTL(post), true, nil
			}
		case missing := <-missingC:
			pending--
			if missing {
				helper.cancel()
				return Post{}, 0, false, igErr(http.StatusNotFound, errorCodeMediaNotFound, "This post isn't available.")
			}
		case <-ctx.Done():
			return answer(contextAppError(ctx))
		}
	}
}

func gateOrRetry(err *AppError, helper *postHelper) *AppError {
	if externalHelperPostImpl != nil && !helper.asked() && (err.Code == errorCodeRestricted || err.Code == errorCodeGeoBlocked) {
		retry := *err
		retry.Code = errorCodeUpstream
		return &retry
	}
	return err
}

func (a *App) publishGate(shortcode string, err *AppError) {
	notice := *err
	a.gateNotices.Store(shortcode, &notice)
	time.AfterFunc(partialPostTTL, func() { a.gateNotices.CompareAndDelete(shortcode, &notice) })
}

func (a *App) storeHelperPost(ctx context.Context, shortcode string, post Post) {
	ctx, cancel := context.WithTimeout(ctx, cacheSharedWorkTimeout)
	defer cancel()
	if post.CreatedAt.IsZero() {
		post.CreatedAt = shortcodeTime(shortcode)
	}
	a.posts.putAfterFlight(ctx, shortcode, post, postCacheTTL(post))
}
