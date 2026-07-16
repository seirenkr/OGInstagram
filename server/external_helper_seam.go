package main

import "context"

// External-helper fallbacks, injected by an init() in the untracked
// external_helper.go. They stay nil when that file is absent; the tracked
// sources alone must build.
var (
	externalHelperPostFallbackFn func(a *App, ctx context.Context, shortcode string) (Post, bool)
	externalHelperStoryFn        func(a *App, ctx context.Context, username, id string) (Story, *AppError)
)
