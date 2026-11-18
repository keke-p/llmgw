// Package gateway runs requests against upstream providers: alias targets and
// fallback, phase-specific timeouts (connect / TTFT / idle / max duration),
// cancellation, keepalive and lazy response-header commit.
//
// Wire formats are handled elsewhere: inbound adapters implement Sink to render
// common events, and the upstream package builds/parses upstream requests.
package gateway

import (
	"io"
	"slices"
	"time"

	"llmgw/internal/chat"
	"llmgw/internal/gwerr"
	"llmgw/internal/store"
	"llmgw/internal/upstream"
)

// Principal is the authenticated caller (a virtual key).
type Principal struct {
	KeyID              int64
	KeyName            string
	AllowedAliases     []string // empty = all
	LogMode            string   // full | metadata_only | none
	NonStreamKeepalive bool
	MaxConcurrency     int // 0 = unlimited (enforced in phase 8)
}

// AllowsAlias reports whether the key may use the alias.
func (p *Principal) AllowsAlias(name string) bool {
	if p == nil || len(p.AllowedAliases) == 0 {
		return true
	}
	for _, cand := range store.CandidateNames(name) {
		if slices.Contains(p.AllowedAliases, cand) {
			return true
		}
	}
	return false
}

// Sink renders a chat response in the inbound wire format.
type Sink interface {
	// StreamContentType is the Content-Type of streaming responses.
	StreamContentType() string
	// Keepalive returns the bytes sent as a streaming keepalive, or nil if the
	// format has no safe keepalive (Ollama NDJSON).
	Keepalive() []byte
	// Event renders one streamed event. When passthrough is true, ev.Raw (if any)
	// is in the sink's own format and should be forwarded (after rewriting the
	// model name); events without Raw are then skipped.
	Event(w io.Writer, ev *chat.Event, passthrough bool) error
	// StreamEnd terminates a successful stream (e.g. "data: [DONE]").
	StreamEnd(w io.Writer, acc *chat.Accumulator) error
	// StreamError reports an error after the stream has started.
	StreamError(w io.Writer, e *gwerr.Error) error
	// Response renders the complete non-streaming response body.
	Response(acc *chat.Accumulator) ([]byte, error)
	// ErrorBody renders an error body.
	ErrorBody(e *gwerr.Error) []byte
}

// EmbedSink renders an embeddings response in the inbound wire format.
type EmbedSink interface {
	// Response renders the body. raw is the upstream body when the plan was a
	// passthrough (same format), otherwise nil.
	Response(res *upstream.EmbedResult, raw []byte) ([]byte, error)
	ErrorBody(e *gwerr.Error) []byte
}

// Meta is request metadata recorded in logs.
type Meta struct {
	Endpoint  string
	ClientIP  string
	UserAgent string
	RawBody   []byte
}

// ChatCall is one chat request to execute.
type ChatCall struct {
	Meta
	Inbound   *upstream.Inbound
	Alias     *store.Alias
	Principal *Principal
	Sink      Sink
}

// EmbedCall is one embeddings request to execute.
type EmbedCall struct {
	Meta
	Inbound   *upstream.EmbedInbound
	Alias     *store.Alias
	Principal *Principal
	Sink      EmbedSink
}

// Attempt is one try against one target (stored in request_logs.attempts).
type Attempt struct {
	ProviderID    int64  `json:"provider_id"`
	ProviderName  string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Result        string `json:"result"` // ok | connect_error | http_error | timeout | stream_error | cancelled | conversion_error
	Status        int    `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
}

// Record is the outcome of one request, written to request_logs.
type Record struct {
	CreatedAt         time.Time
	KeyID             int64
	KeyName           string
	LogMode           string
	Alias             string
	ProviderID        int64
	UpstreamModel     string
	Endpoint          string
	InboundFormat     chat.Format
	UpstreamFormat    chat.Format
	Passthrough       bool
	Stream            bool
	StatusCode        int
	Error             string
	QueueMS           int64
	LatencyMS         int64
	TTFTMS            *int64
	TimeoutPhase      string
	CancelledByClient bool
	Interrupted       bool
	Attempts          []Attempt
	PromptTokens      *int
	CompletionTokens  *int
	ClientIP          string
	UserAgent         string

	RequestBody         []byte
	UpstreamRequestBody []byte
	// Response holds the assembled chat response (may be partial when Interrupted).
	Response *chat.Accumulator
	// ResponseBody is the exact body returned for non-streaming responses
	// (embeddings, non-stream chat) when available.
	ResponseBody []byte
}
