package main

import "time"

type Attachment struct {
	Kind      string
	URL       string
	Thumbnail string
	Width     int
	Height    int

	OversizedInline bool
}

type Post struct {
	Shortcode   string
	Username    string
	FullName    string
	ProfilePic  string
	Caption     string
	StatsLine   string
	Attachments []Attachment
	CreatedAt   time.Time
}

type Config struct {
	Port            int
	Version         string
	DecodoUser      string
	DecodoPass      string
	BrandName       string
	BrandColor      string
	SupportURL      string
	GitHubURL       string
	BaseURL         string
	CacheTTLSeconds int
	HourlyLimit     int
	AssetsDir       string
	LocalPreview    bool
}

type RequestQuery struct {
	ImgIndex    int
	ImgIndexSet bool
	Index       int
	IndexSet    bool
	Order       int
	OrderSet    bool
	PostType    string
	Shortcode   string
	Text        string
	Status      string
	Provider    string
	Gallery     bool
}

type ActivityRoute struct {
	Shortcode           string
	PostType            string
	MediaIndex          int
	MediaIndexSpecified bool
	Gallery             bool
}

type AppError struct {
	Status  int
	Message string
	Reason  string
}

func (e *AppError) Error() string { return e.Message }

func newAppError(status int, message string) *AppError {
	return &AppError{Status: status, Message: message}
}

func igErr(status int, reason, message string) *AppError {
	return &AppError{Status: status, Message: message, Reason: reason}
}

const (
	reasonLoginRequired = "ClientLoginRequired"
	reasonUnauthorized  = "ClientUnauthorizedError"
	reasonForbidden     = "ClientForbiddenError"
	reasonBadRequest    = "ClientBadRequestError"
	reasonThrottled     = "ClientThrottledError"
	reasonNotFound      = "ClientNotFoundError"
	reasonMediaNotFound = "MediaNotFound"
	reasonGraphql       = "ClientGraphqlError"
	reasonJSONDecode    = "ClientJSONDecodeError"
	reasonConnection    = "ClientConnectionError"
	reasonClientError   = "ClientError"
)

func isTransientStatus(status int) bool {
	return status == 401 || status == 403 || status == 429 || status >= 500
}
