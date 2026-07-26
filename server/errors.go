package main

import (
	"context"
	"net/http"
)

const (
	errorCodeConnection    = "connection_error"
	errorCodeJSONDecode    = "json_decode_error"
	errorCodeLoginRequired = "login_required"
	errorCodeUnauthorized  = "unauthorized"
	errorCodeForbidden     = "forbidden"
	errorCodeRateLimited   = "rate_limited"
	errorCodeUpstream      = "upstream_error"
	errorCodeGraphQL       = "graphql_error"
	errorCodeBadRequest    = "bad_request"
	errorCodeNotFound      = "not_found"
	errorCodeMediaNotFound = "media_not_found"

	errorCodeBudgetExhausted = "budget_exhausted"
	errorCodeBudgetBackend   = "budget_backend_error"
	errorCodeGeoBlocked      = "geo_block_required"
)

func shouldRotate(code string) bool {
	switch code {
	case errorCodeConnection, errorCodeJSONDecode, errorCodeLoginRequired, errorCodeUnauthorized,
		errorCodeForbidden, errorCodeRateLimited, errorCodeUpstream:
		return true
	default:
		return false
	}
}

func isTransient(code string) bool {
	switch code {
	case errorCodeBadRequest, errorCodeNotFound, errorCodeMediaNotFound, errorCodeGeoBlocked:
		return false
	default:
		return true
	}
}

func errorCacheSeconds(code string) int {
	if isTransient(code) {
		return transientErrorCacheSeconds
	}
	return permanentErrorCacheSeconds
}

type AppError struct {
	Status        int
	PublicMessage string
	Code          string
	Cause         error `json:"-"`

	Ephemeral bool

	CardCode, CardTitle, CardDesc string
}

func igErr(status int, code, publicMessage string) *AppError {
	return &AppError{Status: status, PublicMessage: publicMessage, Code: code}
}
func ephemeralErr(status int, code, publicMessage string) *AppError {
	return &AppError{Status: status, PublicMessage: publicMessage, Code: code, Ephemeral: true}
}
func causedErr(status int, code, publicMessage string, cause error) *AppError {
	return &AppError{Status: status, PublicMessage: publicMessage, Code: code, Cause: cause}
}
func (e *AppError) logMessage() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.PublicMessage
}
func (e *AppError) errorType() string {
	if e.Code != "" {
		return e.Code
	}
	return "internal_error"
}
func errorCard(kind, errorCode string) (title, desc string) {
	if isTransient(errorCode) {
		return "Temporarily unavailable", "Couldn't load this " + kind + " right now. Please try again in a moment."
	}
	switch kind {
	case "profile":
		return "Account unavailable", "This account isn't available - it may not exist, be deactivated, or the username is incorrect."
	case "story":
		return "Story unavailable", "This story isn't available - stories expire after 24 hours, and it may also be from a private account or the link may be incorrect."
	}
	return "Post unavailable", "This post isn't available - it may be deleted, set to private, or the link is incorrect."
}

func contextAppError(ctx context.Context) *AppError {
	if ctx.Err() == context.DeadlineExceeded {
		return ephemeralErr(http.StatusGatewayTimeout, errorCodeConnection, "upstream deadline exceeded")
	}
	return ephemeralErr(499, "", "cancelled")
}
func preferredError(current, candidate *AppError) *AppError {
	if current == nil || (isTransient(current.Code) && candidate != nil && !isTransient(candidate.Code)) {
		return candidate
	}
	return current
}
