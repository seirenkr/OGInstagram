package main

import "context"

type externalHelperClient interface {
	request(ctx context.Context, operation, endpoint, contentType, unsigned string) (string, *AppError)
	close()
}

var (
	newExternalHelperClient func(*SessionPool) externalHelperClient
	// nil until a private helper registers itself; fetchPost then skips the stage.
	externalHelperPostImpl func(*App, context.Context, string) (Post, bool)

	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		return Story{}, igErr(404, errorCodeMediaNotFound, "story fetching is not available")
	}
)
