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
// the authenticated data-plane route answer with.
const (
	CodeInvalidRequest  = "invalid_request"
	CodeInvalidAPIKey   = "invalid_api_key"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeRequestTooLarge = "request_too_large"
	CodeInternalError   = "internal_error"
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
	case CodeInvalidRequest:
		e.Status = http.StatusBadRequest
	case CodeInvalidAPIKey:
		e.Status, e.Type = http.StatusUnauthorized, "authentication_error"
	case CodeNotFound:
		e.Status, e.Type = http.StatusNotFound, "not_found_error"
	case CodeConflict:
		e.Status = http.StatusConflict
	case CodeRequestTooLarge:
		e.Status = http.StatusRequestEntityTooLarge
	default:
		e.Status, e.Type, e.Code = http.StatusInternalServerError, "api_error", CodeInternalError
	}
	return e
}
