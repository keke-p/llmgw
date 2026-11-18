package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"

	"llmgw/internal/gwerr"
	"llmgw/internal/params"
	"llmgw/internal/store"
	"llmgw/internal/timeouts"
	"llmgw/internal/upstream"
)

// maxEmbedResponse bounds the upstream embeddings response size.
const maxEmbedResponse = 256 << 20

// Embed executes an embeddings request. Embeddings are not streamed, so the TTFT
// timeout bounds the whole upstream response.
func (e *Engine) Embed(w http.ResponseWriter, r *http.Request, c *EmbedCall) {
	rec := e.newRecord(c.Meta, c.Principal, c.Alias.Name, c.Inbound.Format, false)
	defer e.emit(rec)
	fail := func(ge *gwerr.Error) {
		rec.StatusCode, rec.Error, rec.TimeoutPhase = ge.Status, ge.Message, ge.Phase
		rec.ResponseBody = c.Sink.ErrorBody(ge)
		ge.SetRetryAfter(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ge.Status)
		_, _ = w.Write(c.Sink.ErrorBody(ge))
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
	for i, t := range c.Alias.Targets {
		eff := timeouts.Resolve(settings, t.Provider, c.Alias)
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
		body, res := e.embedAttempt(r.Context(), c, t, eff, rec, prm)
		relP()
		if res.clientGone {
			rec.CancelledByClient, rec.StatusCode, rec.Error = true, 499, "client disconnected"
			return
		}
		if res.err == nil {
			rec.StatusCode, rec.ResponseBody = http.StatusOK, body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		if res.fallbackable && i < len(c.Alias.Targets)-1 {
			continue
		}
		fail(res.err)
		return
	}
}

func (e *Engine) embedAttempt(parent context.Context, c *EmbedCall, t store.Target, eff timeouts.Effective,
	rec *Record, prm *params.Params) (body []byte, res attemptResult) {

	start := e.Clock.Now()
	at := Attempt{ProviderID: t.Provider.ID, ProviderName: t.Provider.Name, UpstreamModel: t.UpstreamModel}
	defer func() {
		at.DurationMS = e.Clock.Now().Sub(start).Milliseconds()
		if res.err == nil && !res.clientGone {
			at.Result = "ok"
		} else if res.clientGone {
			at.Result = "cancelled"
		}
		if res.err != nil && at.Error == "" {
			at.Error = res.err.Message
		}
		rec.Attempts = append(rec.Attempts, at)
	}()

	rec.ProviderID, rec.UpstreamModel = t.Provider.ID, t.UpstreamModel
	plan, err := upstream.PlanEmbed(t.Provider, t.UpstreamModel, c.Inbound, prm)
	if err != nil {
		at.Result = "conversion_error"
		var ce *upstream.ConversionError
		if errors.As(err, &ce) {
			return nil, attemptResult{err: gwerr.BadRequest("%s", ce.Msg)}
		}
		return nil, attemptResult{err: gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")}
	}
	rec.UpstreamFormat, rec.Passthrough, rec.UpstreamRequestBody = plan.Format, plan.Passthrough, plan.Body

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	wd := newWatchdog(e.Clock, cancel)
	defer wd.stop()

	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wd.sent(eff.TTFT.D) }}
	req, err := upstream.NewRequest(httptrace.WithClientTrace(ctx, trace), t.Provider, plan.URL, plan.Body)
	if err != nil {
		at.Result = "connect_error"
		return nil, attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "invalid upstream URL"), fallbackable: true}
	}
	wd.arm(gwerr.PhaseConnect, eff.Connect.D)

	resp, err := e.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, e.ctxResult(ctx, parent, eff, &at)
		}
		return nil, classifyItemError(item{err: err, connErr: true}, &at)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, classifyItemError(item{err: upstream.ReadHTTPError(resp)}, &at)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedResponse))
	if err != nil {
		if ctx.Err() != nil {
			return nil, e.ctxResult(ctx, parent, eff, &at)
		}
		return nil, classifyItemError(item{err: err}, &at)
	}
	wd.stop()
	if s := wd.sentTime(); !s.IsZero() {
		ms := e.Clock.Now().Sub(s).Milliseconds()
		rec.TTFTMS = &ms
	}

	result, err := plan.Parse(raw)
	if err != nil {
		at.Result = "stream_error"
		return nil, attemptResult{err: gwerr.New(http.StatusBadGateway, gwerr.API, "%s", err.Error())}
	}
	p := result.PromptTokens
	rec.PromptTokens = &p
	var passRaw []byte
	if plan.Passthrough {
		passRaw = raw
	}
	out, err := c.Sink.Response(result, passRaw)
	if err != nil {
		return nil, attemptResult{err: gwerr.New(http.StatusInternalServerError, gwerr.API, "internal error")}
	}
	return out, attemptResult{}
}
