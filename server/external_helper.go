package main

import "context"

var (
	externalHelperPostImpl = func(*App, context.Context, string) (Post, bool) {
		return Post{}, false
	}

	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		return Story{}, igErr(404, errorCodeMediaNotFound, "story fetching is not available")
	}
)

func (a *App) externalHelperPost(ctx context.Context, shortcode string) (Post, bool) {
	return externalHelperPostImpl(a, ctx, shortcode)
}

func (a *App) externalHelperStory(ctx context.Context, username, id string) (Story, *AppError) {
	return externalHelperStoryImpl(a, ctx, username, id)
}
