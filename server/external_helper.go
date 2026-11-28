package main

import (
	"context"
	"net/http"
)

type externalHelperClient interface{ close() }

var (
	newExternalHelperClient func(*SessionPool) externalHelperClient
	externalHelperPostImpl  func(*App, context.Context, string) (Post, *AppError)

	externalHelperStoryImpl = func(*App, context.Context, string, string) (Story, *AppError) {
		return Story{}, ephemeralErr(http.StatusNotImplemented, errorCodeUpstream, "story fetching is not available")
	}
)
