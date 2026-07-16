package main

const (
	reasonConnection     = "ClientConnectionError"
	reasonJSONDecode     = "ClientJSONDecodeError"
	reasonLoginRequired  = "ClientLoginRequired"
	reasonUnauthorized   = "ClientUnauthorizedError"
	reasonForbidden      = "ClientForbiddenError"
	reasonThrottled      = "ClientThrottledError"
	reasonClientError    = "ClientError"
	reasonGraphql        = "ClientGraphqlError"
	reasonBadRequest     = "ClientBadRequestError"
	reasonNotFound       = "ClientNotFoundError"
	reasonMediaNotFound  = "MediaNotFound"
	reasonBudgetExceeded = "HourlyBudgetExceeded"
	reasonBudgetBackend  = "HourlyBudgetBackendError"

	reasonGeoBlocked = "GeoBlockRequired"
)

func shouldRotate(reason string) bool {
	switch reason {
	case reasonConnection, reasonJSONDecode, reasonLoginRequired, reasonUnauthorized,
		reasonForbidden, reasonThrottled, reasonClientError:
		return true
	default:
		return false
	}
}

func isTransient(reason string) bool {
	switch reason {
	case reasonBadRequest, reasonNotFound, reasonMediaNotFound, reasonGeoBlocked:
		return false
	default:
		return true
	}
}

func errorCacheSeconds(reason string) int {
	if isTransient(reason) {
		return transientErrorCacheSeconds
	}
	return permanentErrorCacheSeconds
}
