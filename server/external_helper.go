package main

import (
	"context"
	"net/http"
)

type externalHelperClient interface {
	request(ctx context.Context, operation, endpoint, contentType, unsigned string) (string, *AppError)
	close()
}

var (
	newExternalHelperClient func(*SessionPool) externalHelperClient
	// nil until a private helper registers itself; fetchPost then skips the stage.
	externalHelperPostImpl func(*App, context.Context, string) (Post, *AppError)

	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		return Story{}, ephemeralErr(http.StatusNotImplemented, errorCodeUpstream, "story fetching is not available")
	}
)
