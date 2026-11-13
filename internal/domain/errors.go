package domain

import "net/http"

// Severities of a finding: an error refuses a change, a warning never blocks.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Finding is one result of a check on a configuration. Rule is a kick-off rule
// id and Resource a path such as "models/fast".
type Finding struct {
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Code     string `json:"code"`
	Resource string `json:"resource"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
}

// RequestIDHeader carries the request id on every response of both listeners
// (kick-off section 15).
const RequestIDHeader = "X-Stargaite-Request-Id"

// Error codes of the error model (kick-off section 15) that the admin API and
// the data plane answer with. The limits of phase 3 add theirs.
const (
	CodeInvalidRequest  = "invalid_request"
	CodeInvalidAPIKey   = "invalid_api_key"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeRequestTooLarge = "request_too_large"
	CodeInternalError   = "internal_error"

	CodeUnknownParameter        = "unknown_parameter"
	CodeInvalidReasoningEffort  = "invalid_reasoning_effort"
	CodeModelNotAllowed         = "model_not_allowed"
	CodeModelNotFound           = "model_not_found"
	CodeEndpointNotSupported    = "endpoint_not_supported"
	CodeCapabilityMismatch      = "capability_mismatch"
	CodeMaxOutputTokensExceeded = "max_output_tokens_exceeded"
	CodeUnsupportedParameter    = "unsupported_parameter"
	CodeUpstreamError           = "upstream_error"
	CodeUpstreamAuthError       = "upstream_auth_error"
	CodeNoTargetAvailable       = "no_target_available"
	CodeUpstreamTimeout         = "upstream_timeout"
)

// Endpoint is an inference endpoint: the format of the inbound body.
type Endpoint string

const (
	EndpointChat     Endpoint = "chat"     // POST /v1/chat/completions
	EndpointMessages Endpoint = "messages" // POST /v1/messages
)

// Error is the error object of the response envelope, {"error": {...}}. The
// members are in the order of the OpenAI envelope the error model adopts.
type Error struct {
	Status   int       `json:"-"`
	Message  string    `json:"message"`
	Type     string    `json:"type"`
	Param    string    `json:"param,omitempty"`
	Code     string    `json:"code"`
	Findings []Finding `json:"findings,omitempty"`
	// RetryAfter is written as the Retry-After header by both envelopes: the
	// provider's own value on a 429 passed through (RES-1).
	RetryAfter string `json:"-"`
}

// Error returns the message, so a *Error travels as an ordinary error until
// the listener writes it.
func (e *Error) Error() string { return e.Message }

// NewError returns the error of code with the status and type the error model
// fixes for it. A code outside the list above becomes an internal error, so a
// mistyped code can never answer with a success status or an empty type.
func NewError(code, message string) *Error {
	e := &Error{Message: message, Type: "invalid_request_error", Code: code}
	switch code {
	case CodeInvalidRequest, CodeUnknownParameter, CodeInvalidReasoningEffort:
		e.Status = http.StatusBadRequest
	case CodeInvalidAPIKey:
		e.Status, e.Type = http.StatusUnauthorized, "authentication_error"
	case CodeModelNotAllowed:
		e.Status, e.Type = http.StatusForbidden, "permission_error"
	case CodeNotFound, CodeModelNotFound:
		e.Status, e.Type = http.StatusNotFound, "not_found_error"
	case CodeConflict:
		e.Status = http.StatusConflict
	case CodeRequestTooLarge:
		e.Status = http.StatusRequestEntityTooLarge
	case CodeEndpointNotSupported, CodeCapabilityMismatch, CodeMaxOutputTokensExceeded, CodeUnsupportedParameter:
		e.Status = http.StatusUnprocessableEntity
	case CodeUpstreamError, CodeUpstreamAuthError:
		e.Status, e.Type = http.StatusBadGateway, "api_error"
	case CodeNoTargetAvailable:
		e.Status, e.Type = http.StatusServiceUnavailable, "overloaded_error"
	case CodeUpstreamTimeout:
		e.Status, e.Type = http.StatusGatewayTimeout, "api_error"
	default:
		e.Status, e.Type, e.Code = http.StatusInternalServerError, "api_error", CodeInternalError
	}
	return e
}

// UpstreamError is the error answered for a provider's own status once no
// target is left (kick-off section 15): code upstream_error with the
// provider's status and the type of that status for 400, 404, 413, 422 and
// 429, else 502. retryAfter is kept on a 429 only.
func UpstreamError(status int, message, retryAfter string) *Error {
	e := NewError(CodeUpstreamError, message)
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		e.Status, e.Type = status, "invalid_request_error"
	case http.StatusNotFound:
		e.Status, e.Type = status, "not_found_error"
	case http.StatusTooManyRequests:
		e.Status, e.Type, e.RetryAfter = status, "rate_limit_error", retryAfter
	}
	return e
}

// Usage is what a provider reports a call consumed (USE-3): Input and Output
// are inclusive totals, the others are parts of them.
type Usage struct {
	Input      int64  `json:"input"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Output     int64  `json:"output"`
	Reasoning  *int64 `json:"reasoning,omitempty"` // nil when not reported
}

// Observed is what the relay read from one provider response, the input of
// usage accounting (phase 3).
type Observed struct {
	ResponseModel string // USE-4
	FinishReason  string // the provider's own value (USE-3)
	Usage         *Usage // nil when none arrived: phase 3 estimates (USE-8)
	VisibleBytes  int64  // visible text relayed, for the estimate
	ToolCalls     int    // with VisibleBytes 0: content_empty (USE-9)
	Stripped      bool   // RSN-12
}
