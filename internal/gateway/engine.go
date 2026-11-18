package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"time"

	"llmgw/internal/chat"
	"llmgw/internal/clock"
	"llmgw/internal/gwerr"
	"llmgw/internal/limits"
	"llmgw/internal/params"
	"llmgw/internal/store"
	"llmgw/internal/timeouts"
	"llmgw/internal/upstream"
)

// Engine executes gateway requests.
type Engine struct {
	Store  *store.Store
	Client *http.Client
	Clock  clock.Clock
	Log    *slog.Logger
	// Record receives the outcome of every request (request logging).
	Record func(*Record)
	// ClientWriteTimeout bounds each write to the client, so a stalled client
	// cannot block the upstream reader forever.
	ClientWriteTimeout time.Duration
	// Limits enforces key and provider max_concurrency (nil = unlimited).
	Limits *limits.Registry
}

// acquire takes a concurrency slot, waiting FIFO up to timeout. The wait is
// added to rec.QueueMS. It returns the release func, or a queue-timeout error,
// or clientGone when the client disconnected while queued.
func (e *Engine) acquire(ctx context.Context, name string, limit int, timeout time.Duration, rec *Record) (release func(), ge *gwerr.Error, clientGone bool) {
	if e.Limits == nil || limit <= 0 {
		return func() {}, nil, false
	}
	start := e.Clock.Now()
	rel, err := e.Limits.Acquire(ctx, name, limit, timeout)
	rec.QueueMS += e.Clock.Now().Sub(start).Milliseconds()
	switch {
	case err == nil:
		return rel, nil, false
	case errors.Is(err, limits.ErrQueueTimeout):
		return nil, gwerr.TimeoutError(gwerr.PhaseQueue, timeout), false
	default:
		return nil, nil, true
	}
}

func keySlot(p *Principal) (string, int) {
	if p == nil || p.KeyID <= 0 {
		return "", 0
	}
	return "key:" + strconv.FormatInt(p.KeyID, 10), p.MaxConcurrency
}

func providerSlot(p *store.Provider) (string, int) {
	return "provider:" + strconv.FormatInt(p.ID, 10), p.MaxConcurrency
}

// NewHTTPClient returns the upstream HTTP client. It has no overall Timeout
// (that would cut streams); phase timeouts are enforced by the watchdog.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: nil, // upstreams are local; ignore HTTP(S)_PROXY
		DialContext: (&net.Dialer{
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 0, // covered by connect_timeout
		DisableCompression:  true,
	}}
}

// ResolveAlias resolves a client model name for a principal.
func (e *Engine) ResolveAlias(ctx context.Context, p *Principal, model string) (*store.Alias, *gwerr.Error) {
	if model == "" {
		return nil, gwerr.BadRequest("model is required")
	}
	a, err := e.Store.ResolveAlias(ctx, model)
	if errors.Is(err, store.ErrNotFound) {
		return nil, gwerr.ModelNotFound(model)
	}
	if err != nil {
		e.Log.Error("resolve alias", "err", err, "model", model)
		return nil, gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")
	}
	if !p.AllowsAlias(a.Name) {
		return nil, gwerr.New(http.StatusForbidden, gwerr.Permission, "this key is not allowed to use model %q", model)
	}
	return a, nil
}

func (e *Engine) newRecord(m Meta, p *Principal, alias string, f chat.Format, stream bool) *Record {
	rec := &Record{
		CreatedAt: e.Clock.Now(), Alias: alias, Endpoint: m.Endpoint, InboundFormat: f,
		Stream: stream, ClientIP: m.ClientIP, UserAgent: m.UserAgent, RequestBody: m.RawBody,
	}
	if p != nil {
		rec.KeyID, rec.KeyName, rec.LogMode = p.KeyID, p.KeyName, p.LogMode
	}
	return rec
}

func (e *Engine) emit(rec *Record) {
	rec.LatencyMS = e.Clock.Now().Sub(rec.CreatedAt).Milliseconds()
	if e.Record != nil {
		e.Record(rec)
	}
}

// RecordError records a request that failed before reaching the engine
// (bad request, unknown model, auth failure, ...).
func (e *Engine) RecordError(m Meta, p *Principal, f chat.Format, model string, ge *gwerr.Error) {
	rec := e.newRecord(m, p, model, f, false)
	rec.StatusCode, rec.Error = ge.Status, ge.Message
	e.emit(rec)
}

// ---- chat ------------------------------------------------------------------

type attemptResult struct {
	err          *gwerr.Error
	fallbackable bool
	clientGone   bool
}

// Chat executes a chat request and writes the response.
func (e *Engine) Chat(w http.ResponseWriter, r *http.Request, c *ChatCall) {
	stream := c.Inbound.Req.Stream
	rec := e.newRecord(c.Meta, c.Principal, c.Alias.Name, c.Inbound.Format, stream)
	acc := chat.NewAccumulator()
	rec.Response = acc
	defer e.emit(rec)
	// For request logs, record the response the client received in its own
	// format: streams are assembled into the equivalent complete response, and
	// partial output of interrupted requests is kept (marked Interrupted).
	defer func() {
		if rec.LogMode != store.LogFull || !acc.HasOutput() {
			return
		}
		if stream || rec.ResponseBody == nil || rec.StatusCode != http.StatusOK {
			if b, err := c.Sink.Response(acc); err == nil {
				rec.ResponseBody = b
			}
		}
	}()

	out := &output{w: w, rc: http.NewResponseController(w), writeTimeout: e.clientWriteTimeout()}
	fail := func(ge *gwerr.Error) {
		rec.StatusCode, rec.Error, rec.TimeoutPhase = ge.Status, ge.Message, ge.Phase
		if out.committed {
			rec.StatusCode = out.status
			rec.Interrupted = true
		} else {
			rec.ResponseBody = c.Sink.ErrorBody(ge)
		}
		e.writeChatError(out, c.Sink, stream, ge)
	}

	settings, err := e.Store.Settings(r.Context())
	if err != nil {
		e.Log.Error("load settings", "err", err)
		fail(gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error"))
		return
	}
	if len(c.Alias.Targets) == 0 {
		fail(gwerr.New(http.StatusServiceUnavailable, gwerr.Overloaded, "no available upstream for model %q", c.Alias.Name))
		return
	}

	prm := params.Parse(c.Alias.Params)
	queuedOut := func() {
		rec.CancelledByClient, rec.StatusCode, rec.Error = true, 499, "client disconnected while queued"
	}
	// Key slot: held for the whole request, released once the last upstream
	// connection is closed (attempts return only after that).
	if name, limit := keySlot(c.Principal); limit > 0 {
		first := timeouts.Resolve(settings, c.Alias.Targets[0].Provider, c.Alias)
		rel, ge, gone := e.acquire(r.Context(), name, limit, first.Queue.D, rec)
		if gone {
			queuedOut()
			return
		}
		if ge != nil {
			fail(ge)
			return
		}
		defer rel()
	}

	var deadline time.Time
	started := false
	for i, t := range c.Alias.Targets {
		eff := timeouts.Resolve(settings, t.Provider, c.Alias)
		// Provider slot: per attempt; queue time does not count towards max_duration.
		name, limit := providerSlot(t.Provider)
		relP, ge, gone := e.acquire(r.Context(), name, limit, eff.Queue.D, rec)
		if gone {
			queuedOut()
			return
		}
		if ge != nil {
			fail(ge)
			return
		}
		if !started {
			started = true
			if eff.MaxDuration.D > 0 {
				deadline = e.Clock.Now().Add(eff.MaxDuration.D)
			}
		}
		if i > 0 {
			acc.Reset()
		}
		res := e.chatAttempt(r.Context(), c, t, eff, deadline, out, acc, rec, prm)
		relP() // the upstream connection is closed when chatAttempt returns
		if res.err == nil && !res.clientGone {
			rec.StatusCode = out.status
			e.fillUsage(rec, acc)
			return
		}
		if res.clientGone {
			rec.CancelledByClient, rec.Interrupted = true, acc.HasOutput()
			rec.StatusCode = 499
			if out.committed {
				rec.StatusCode = out.status
			}
			rec.Error = "client disconnected"
			e.fillUsage(rec, acc)
			return
		}
		if res.fallbackable && !out.tokensSent && i < len(c.Alias.Targets)-1 {
			e.Log.Warn("upstream attempt failed; falling back", "alias", c.Alias.Name,
				"provider", t.Provider.Name, "err", res.err.Message)
			continue
		}
		rec.Interrupted = acc.HasOutput()
		e.fillUsage(rec, acc)
		fail(res.err)
		return
	}
}

func (e *Engine) fillUsage(rec *Record, acc *chat.Accumulator) {
	if acc.Usage != nil {
		p, c := acc.Usage.PromptTokens, acc.Usage.CompletionTokens
		rec.PromptTokens, rec.CompletionTokens = &p, &c
	}
}

func (e *Engine) clientWriteTimeout() time.Duration {
	if e.ClientWriteTimeout > 0 {
		return e.ClientWriteTimeout
	}
	return 60 * time.Second
}

type item struct {
	ev      *chat.Event
	err     error
	connErr bool // error from client.Do (before any response)
	end     bool // parse finished (err == nil means success)
}

func (e *Engine) chatAttempt(parent context.Context, c *ChatCall, t store.Target, eff timeouts.Effective,
	deadline time.Time, out *output, acc *chat.Accumulator, rec *Record, prm *params.Params) (res attemptResult) {

	start := e.Clock.Now()
	at := Attempt{ProviderID: t.Provider.ID, ProviderName: t.Provider.Name, UpstreamModel: t.UpstreamModel}
	defer func() {
		at.DurationMS = e.Clock.Now().Sub(start).Milliseconds()
		switch {
		case res.clientGone:
			at.Result = "cancelled"
		case res.err == nil:
			at.Result = "ok"
		}
		if res.err != nil && at.Error == "" {
			at.Error = res.err.Message
		}
		rec.Attempts = append(rec.Attempts, at)
	}()

	rec.ProviderID, rec.UpstreamModel = t.Provider.ID, t.UpstreamModel
	plan, err := upstream.PlanChat(t.Provider, t.UpstreamModel, c.Inbound, prm)
	if err != nil {
		at.Result = "conversion_error"
		var ce *upstream.ConversionError
		if errors.As(err, &ce) {
			return attemptResult{err: gwerr.BadRequest("%s", ce.Msg)}
		}
		e.Log.Error("plan upstream request", "err", err)
		return attemptResult{err: gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")}
	}
	rec.UpstreamFormat, rec.Passthrough, rec.UpstreamRequestBody = plan.Format, plan.Passthrough, plan.Body

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	wd := newWatchdog(e.Clock, cancel)
	defer wd.stop()

	if !deadline.IsZero() {
		remaining := deadline.Sub(e.Clock.Now())
		if remaining <= 0 {
			at.Result = "timeout"
			return attemptResult{err: gwerr.TimeoutError(gwerr.PhaseMaxDuration, eff.MaxDuration.D)}
		}
		wd.armLimit(gwerr.PhaseMaxDuration, remaining, eff.MaxDuration.D)
	}
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wd.sent(eff.TTFT.D) }}
	req, err := upstream.NewRequest(httptrace.WithClientTrace(ctx, trace), t.Provider, plan.URL, plan.Body)
	if err != nil {
		at.Result = "connect_error"
		return attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "invalid upstream URL"), fallbackable: true}
	}
	for k, v := range plan.Header {
		req.Header[k] = v
	}
	wd.arm(gwerr.PhaseConnect, eff.Connect.D)

	items := make(chan item, 16)
	go func() {
		defer close(items)
		send := func(it item) bool {
			select {
			case items <- it:
				return true
			case <-ctx.Done():
				return false
			}
		}
		resp, err := e.Client.Do(req)
		if err != nil {
			send(item{err: err, connErr: true})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			send(item{err: upstream.ReadHTTPError(resp)})
			return
		}
		perr := plan.Parse(resp, func(ev *chat.Event) error {
			if !send(item{ev: ev}) {
				return context.Cause(ctx)
			}
			return nil
		})
		send(item{err: perr, end: true})
	}()
	// drain waits until the upstream goroutine has exited, i.e. the upstream
	// response body is closed. Concurrency slots must only be released after this.
	drain := func() {
		for range items {
		}
	}

	stream := c.Inbound.Req.Stream
	kaBytes := c.Sink.Keepalive()
	kaEnabled := eff.Keepalive > 0 && ((stream && kaBytes != nil) || (!stream && c.Principal != nil && c.Principal.NonStreamKeepalive))
	kaC := make(chan struct{}, 1)
	var kaTimer clock.Timer
	if kaEnabled {
		kaTimer = e.Clock.AfterFunc(eff.Keepalive, func() {
			select {
			case kaC <- struct{}{}:
			default:
			}
		})
		defer kaTimer.Stop()
	}
	resetKA := func() {
		if kaTimer != nil {
			kaTimer.Reset(eff.Keepalive)
		}
	}

	var pending []*chat.Event
	gotToken := false
	var buf bytes.Buffer
	writeEvent := func(ev *chat.Event) error {
		buf.Reset()
		if err := c.Sink.Event(&buf, ev, plan.Passthrough); err != nil {
			return err
		}
		if buf.Len() == 0 {
			return nil
		}
		out.commit(c.Sink.StreamContentType(), true)
		if err := out.write(buf.Bytes()); err != nil {
			return err
		}
		resetKA()
		return nil
	}
	clientGone := func() attemptResult {
		cancel(errClientGone)
		drain()
		return attemptResult{clientGone: true}
	}

	for {
		select {
		case it, ok := <-items:
			if !ok {
				// The goroutine stopped without a final item: context cancelled.
				return e.ctxResult(ctx, parent, eff, &at)
			}
			if it.ev != nil {
				ev := it.ev
				if ev.IsToken() {
					if !gotToken {
						gotToken = true
						acc.FirstTokenAt = e.Clock.Now()
						if s := wd.sentTime(); !s.IsZero() {
							ms := e.Clock.Now().Sub(s).Milliseconds()
							rec.TTFTMS = &ms
						}
						wd.disarm(gwerr.PhaseTTFT)
					}
					wd.arm(gwerr.PhaseIdle, eff.Idle.D)
				} else if gotToken && ev.IsTerminal() {
					wd.arm(gwerr.PhaseIdle, eff.Idle.D)
				}
				acc.Add(ev)
				if !stream {
					continue
				}
				if !gotToken {
					pending = append(pending, ev)
					continue
				}
				for _, p := range pending {
					if writeEvent(p) != nil {
						return clientGone()
					}
				}
				pending = nil
				if writeEvent(ev) != nil {
					return clientGone()
				}
				out.tokensSent = true
				continue
			}

			// Terminal item.
			if ctx.Err() != nil {
				drain()
				return e.ctxResult(ctx, parent, eff, &at)
			}
			wd.stop()
			drain()
			if it.end && it.err == nil {
				if stream {
					for _, p := range pending {
						if writeEvent(p) != nil {
							return clientGone()
						}
					}
					out.commit(c.Sink.StreamContentType(), true)
					buf.Reset()
					_ = c.Sink.StreamEnd(&buf, acc)
					if out.write(buf.Bytes()) != nil {
						return clientGone()
					}
				} else {
					body, err := c.Sink.Response(acc)
					if err != nil {
						e.Log.Error("render response", "err", err)
						return attemptResult{err: gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")}
					}
					rec.ResponseBody = body
					out.commit("application/json", false)
					if out.write(body) != nil {
						return attemptResult{clientGone: true}
					}
				}
				return attemptResult{}
			}
			return classifyItemError(it, &at)

		case <-kaC:
			ka := kaBytes
			if stream {
				out.commit(c.Sink.StreamContentType(), true)
			} else {
				// Leading whitespace is valid before a JSON document.
				ka = []byte("\n")
				out.commitJSON()
			}
			if out.write(ka) != nil {
				return clientGone()
			}
			resetKA()

		case <-ctx.Done():
			drain()
			return e.ctxResult(ctx, parent, eff, &at)
		}
	}
}

// ctxResult classifies a cancelled attempt context.
func (e *Engine) ctxResult(ctx, parent context.Context, eff timeouts.Effective, at *Attempt) attemptResult {
	cause := context.Cause(ctx)
	var tc *timeoutCause
	if errors.As(cause, &tc) {
		at.Result = "timeout"
		ge := gwerr.TimeoutError(tc.Phase, tc.Limit)
		switch tc.Phase {
		case gwerr.PhaseConnect:
			return attemptResult{err: ge, fallbackable: true}
		case gwerr.PhaseTTFT:
			return attemptResult{err: ge, fallbackable: eff.FallbackOnTTFT}
		}
		return attemptResult{err: ge}
	}
	if parent.Err() != nil || errors.Is(cause, errClientGone) {
		return attemptResult{clientGone: true}
	}
	at.Result = "cancelled"
	return attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "request cancelled")}
}

func classifyItemError(it item, at *Attempt) attemptResult {
	var he *upstream.HTTPError
	var se *upstream.StreamError
	switch {
	case it.connErr:
		at.Result = "connect_error"
		at.Error = it.err.Error()
		return attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "upstream connection failed"), fallbackable: true}
	case errors.As(it.err, &he):
		at.Result, at.Status = "http_error", he.Status
		return attemptResult{err: upstreamHTTPError(he), fallbackable: he.Status >= 500}
	case errors.As(it.err, &se):
		at.Result = "stream_error"
		return attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "upstream error: %s", se.Message)}
	case errors.Is(it.err, io.ErrUnexpectedEOF):
		at.Result = "stream_error"
		return attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "upstream closed the stream unexpectedly")}
	default:
		at.Result = "stream_error"
		at.Error = it.err.Error()
		return attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "upstream stream failed: %v", it.err)}
	}
}

// upstreamHTTPError maps an upstream HTTP error to a client-facing error.
func upstreamHTTPError(he *upstream.HTTPError) *gwerr.Error {
	msg := fmt.Sprintf("upstream error (%d): %s", he.Status, he.Message)
	switch {
	case he.Status >= 500:
		return &gwerr.Error{Status: http.StatusBadGateway, Kind: gwerr.API, Message: msg}
	case he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden:
		// Upstream credentials are the gateway's problem, not the client's.
		return &gwerr.Error{Status: http.StatusBadGateway, Kind: gwerr.API, Message: msg}
	case he.Status == http.StatusNotFound:
		return &gwerr.Error{Status: http.StatusNotFound, Kind: gwerr.NotFound, Message: msg}
	case he.Status == http.StatusTooManyRequests:
		return &gwerr.Error{Status: http.StatusTooManyRequests, Kind: gwerr.RateLimit, Message: msg}
	default:
		return &gwerr.Error{Status: he.Status, Kind: gwerr.InvalidRequest, Message: msg}
	}
}

func (e *Engine) writeChatError(out *output, sink Sink, stream bool, ge *gwerr.Error) {
	if !out.committed {
		ge.SetRetryAfter(out.w.Header())
		out.w.Header().Set("Content-Type", "application/json")
		out.status, out.committed = ge.Status, true
		out.w.WriteHeader(ge.Status)
		_ = out.write(sink.ErrorBody(ge))
		return
	}
	if stream {
		var buf bytes.Buffer
		_ = sink.StreamError(&buf, ge)
		_ = out.write(buf.Bytes())
		return
	}
	// Non-stream keepalive already committed 200: return the error JSON as body.
	_ = out.write(sink.ErrorBody(ge))
}

// ---- output ----------------------------------------------------------------

// output writes to the client and commits headers lazily: only on the first
// token, the first keepalive, or the final response.
type output struct {
	w            http.ResponseWriter
	rc           *http.ResponseController
	writeTimeout time.Duration
	committed    bool
	status       int
	tokensSent   bool
	failed       error
}

func (o *output) commit(contentType string, stream bool) {
	if o.committed {
		return
	}
	h := o.w.Header()
	h.Set("Content-Type", contentType)
	if stream {
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
	}
	o.w.WriteHeader(http.StatusOK)
	o.committed, o.status = true, http.StatusOK
}

func (o *output) commitJSON() { o.commit("application/json", false) }

func (o *output) write(b []byte) error {
	if o.failed != nil {
		return o.failed
	}
	if len(b) == 0 {
		return nil
	}
	_ = o.rc.SetWriteDeadline(time.Now().Add(o.writeTimeout))
	if _, err := o.w.Write(b); err != nil {
		o.failed = err
		return err
	}
	if err := o.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		o.failed = err
		return err
	}
	return nil
}
