package main

import "context"

var (
	// nil until a private helper registers itself; fetchPost then skips the stage.
	externalHelperPostImpl func(*App, context.Context, string) (Post, bool)

	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		return Story{}, igErr(404, errorCodeMediaNotFound, "story fetching is not available")
	}
)
