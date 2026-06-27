package main

const (
	reasonConnection     = "ClientConnectionError"
	reasonIncompleteRead = "ClientIncompleteReadError"
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
)

// reasonInfo declares how we treat each classification:
//   - rotateIP:  the proxy session/IP looks at fault → rotate + cooldown
//   - transient: not a permanent fact → short cache, "Temporarily unavailable" card
type reasonInfo struct {
	rotateIP  bool
	transient bool
}

var reasonRegistry = map[string]reasonInfo{
	reasonConnection:     {rotateIP: true, transient: true},
	reasonIncompleteRead: {rotateIP: true, transient: true},
	reasonJSONDecode:     {rotateIP: true, transient: true},
	reasonLoginRequired:  {rotateIP: true, transient: true},
	reasonUnauthorized:   {rotateIP: true, transient: true},
	reasonForbidden:      {rotateIP: true, transient: true},
	reasonThrottled:      {rotateIP: true, transient: true},
	reasonClientError:    {rotateIP: false, transient: true},
	reasonGraphql:        {rotateIP: false, transient: true},
	reasonBadRequest:     {rotateIP: false, transient: false},
	reasonNotFound:       {rotateIP: false, transient: false},
	reasonMediaNotFound:  {rotateIP: false, transient: false},
}

func reasonOf(reason string) reasonInfo {
	if r, ok := reasonRegistry[reason]; ok {
		return r
	}
	return reasonInfo{rotateIP: false, transient: true}
}

func shouldRotate(reason string) bool { return reasonOf(reason).rotateIP }
func isTransient(reason string) bool  { return reasonOf(reason).transient }

func errorCacheSeconds(reason string) int {
	if isTransient(reason) {
		return transientErrorCacheSecond
	}
	return permanentErrorCacheSecond
}
