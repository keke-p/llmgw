// Package gwerr defines the format-independent gateway error. Each inbound
// adapter renders it in its own wire format (OpenAI / Ollama / Anthropic).
package gwerr

import (
	"fmt"
	"net/http"
	"time"
)

// Kind classifies an error independently of the wire format.
type Kind string

const (
	InvalidRequest Kind = "invalid_request"
	Authentication Kind = "authentication"
	Permission     Kind = "permission"
	NotFound       Kind = "not_found"
	RateLimit      Kind = "rate_limit"
	API            Kind = "api_error"
	Overloaded     Kind = "overloaded" // queue timeout / no capacity
	Timeout        Kind = "timeout"
)

// Timeout phases (also stored in request_logs.timeout_phase).
const (
	PhaseQueue       = "queue"
	PhaseConnect     = "connect"
	PhaseTTFT        = "ttft"
	PhaseIdle        = "idle"
	PhaseMaxDuration = "max_duration"
)

// Error is a gateway error with an HTTP status.
type Error struct {
	Status     int
	Kind       Kind
	Message    string
	Code       string        // optional machine-readable code, e.g. "model_not_found"
	Phase      string        // timeout phase, if Kind == Timeout or Overloaded(queue)
	RetryAfter time.Duration // sets Retry-After when > 0
}

func (e *Error) Error() string { return e.Message }

func New(status int, kind Kind, format string, args ...any) *Error {
	return &Error{Status: status, Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func BadRequest(format string, args ...any) *Error {
	return New(http.StatusBadRequest, InvalidRequest, format, args...)
}

func ModelNotFound(name string) *Error {
	e := New(http.StatusNotFound, NotFound, "model %q not found", name)
	e.Code = "model_not_found"
	return e
}

// TimeoutError builds the error for a timeout in the given phase.
func TimeoutError(phase string, limit time.Duration) *Error {
	msg := fmt.Sprintf("timeout: %s exceeded %s", phase, fmtDur(limit))
	if phase == PhaseQueue {
		return &Error{Status: http.StatusServiceUnavailable, Kind: Overloaded, Message: msg,
			Code: "timeout", Phase: phase, RetryAfter: 10 * time.Second}
	}
	return &Error{Status: http.StatusGatewayTimeout, Kind: Timeout, Message: msg, Code: "timeout", Phase: phase}
}

func fmtDur(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	return d.String()
}

// SetRetryAfter writes the Retry-After header when applicable.
func (e *Error) SetRetryAfter(h http.Header) {
	if e.RetryAfter > 0 {
		secs := int64((e.RetryAfter + time.Second - 1) / time.Second)
		h.Set("Retry-After", fmt.Sprint(secs))
	}
}
