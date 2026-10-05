package main

import "context"

var (
	externalHelperSite string
	seedSignCreds      = func(string, string) bool { return false }

	externalHelperPostImpl = func(*App, context.Context, string) (Post, bool) {
		return Post{}, false
	}

	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		return Story{}, igErr(404, errorCodeMediaNotFound, "story fetching is not available")
	}
)
