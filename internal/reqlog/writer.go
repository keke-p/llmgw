package reqlog

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"llmgw/internal/clock"
	"llmgw/internal/gateway"
	"llmgw/internal/store"
)

const (
	queueSize     = 4096
	batchSize     = 100
	flushInterval = 500 * time.Millisecond
	retentionTick = time.Hour
)

// Writer stores request records asynchronously. Record never blocks the
// request: when the queue is full the record is dropped (and counted).
type Writer struct {
	Store *store.Store
	Log   *slog.Logger
	Clock clock.Clock

	ch      chan *gateway.Record
	dropped atomic.Int64
	wg      sync.WaitGroup
	mu      sync.RWMutex // guards closed / channel close against late Record calls
	closed  bool
}

// NewWriter returns a Writer; call Start to begin writing.
func NewWriter(st *store.Store, log *slog.Logger, c clock.Clock) *Writer {
	return &Writer{Store: st, Log: log, Clock: c, ch: make(chan *gateway.Record, queueSize)}
}

// Record enqueues a record (non-blocking).
func (w *Writer) Record(rec *gateway.Record) {
	if rec.LogMode == store.LogNone {
		return
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return
	}
	select {
	case w.ch <- rec:
	default:
		if n := w.dropped.Add(1); n == 1 || n%1000 == 0 {
			w.Log.Warn("request log queue full; dropping records", "dropped_total", n)
		}
	}
}

// Dropped returns the number of records dropped because the queue was full.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Start runs the batch writer and the retention job (until ctx is cancelled).
// Stop flushes the queue and waits for both to finish.
func (w *Writer) Start(ctx context.Context) {
	w.wg.Add(2)
	go func() { defer w.wg.Done(); w.loop() }()
	go func() { defer w.wg.Done(); w.retentionLoop(ctx) }()
}

// Stop flushes pending records. Records arriving afterwards are ignored.
func (w *Writer) Stop() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

func (w *Writer) loop() {
	batch := make([]*gateway.Record, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	flush := func() {
		if len(batch) > 0 {
			w.write(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case rec, ok := <-w.ch:
			if !ok { // Stop: flush and exit
				flush()
				return
			}
			batch = append(batch, rec)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (w *Writer) write(batch []*gateway.Record) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	settings, err := w.Store.Settings(ctx)
	if err != nil {
		settings = store.DefaultSettings()
	}
	rows := make([]*store.LogRow, 0, len(batch))
	for _, rec := range batch {
		rows = append(rows, ToRow(rec, settings))
	}
	if err := w.Store.InsertLogs(ctx, rows); err != nil {
		w.Log.Error("write request logs", "err", err, "count", len(rows))
	}
}

// ToRow converts a record to a DB row, applying the key's log mode and the
// body settings.
func ToRow(rec *gateway.Record, s store.Settings) *store.LogRow {
	r := &store.LogRow{
		CreatedAt: rec.CreatedAt, KeyName: rec.KeyName, Alias: rec.Alias, UpstreamModel: rec.UpstreamModel,
		Endpoint: rec.Endpoint, InboundFormat: string(rec.InboundFormat), UpstreamFormat: string(rec.UpstreamFormat),
		Passthrough: rec.Passthrough, Stream: rec.Stream, StatusCode: rec.StatusCode, Error: rec.Error,
		TTFTMS: rec.TTFTMS, TimeoutPhase: rec.TimeoutPhase, CancelledByClient: rec.CancelledByClient,
		Interrupted: rec.Interrupted, PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens,
		ClientIP: rec.ClientIP, UserAgent: rec.UserAgent,
	}
	if rec.KeyID > 0 {
		id := rec.KeyID
		r.KeyID = &id
	}
	if rec.ProviderID > 0 {
		id := rec.ProviderID
		r.ProviderID = &id
	}
	lat, q := rec.LatencyMS, rec.QueueMS
	r.LatencyMS = &lat
	if q > 0 {
		r.QueueMS = &q
	}
	if len(rec.Attempts) > 0 {
		b, _ := json.Marshal(rec.Attempts)
		r.AttemptsJSON = string(b)
	}
	if rec.LogMode != store.LogFull {
		return r // metadata_only (and unauthenticated requests): no bodies
	}
	body := func(b []byte) *string {
		if len(b) == 0 {
			return nil
		}
		s, trunc := Sanitize(b, s.LogBodyMaxBytes)
		if trunc {
			r.BodyTruncated = true
		}
		return &s
	}
	r.RequestBody = body(rec.RequestBody)
	r.ResponseBody = body(rec.ResponseBody)
	if s.LogUpstreamRequest {
		r.UpstreamRequestBody = body(rec.UpstreamRequestBody)
	}
	return r
}

func (w *Writer) retentionLoop(ctx context.Context) {
	w.purge(ctx)
	t := time.NewTicker(retentionTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.purge(ctx)
		}
	}
}

// Purge deletes logs older than the retention period (and expired sessions).
func (w *Writer) purge(ctx context.Context) {
	s, err := w.Store.Settings(ctx)
	if err != nil {
		return
	}
	now := w.Clock.Now()
	_ = w.Store.DeleteExpiredSessions(ctx, now)
	if s.LogRetentionDays <= 0 {
		return
	}
	n, err := w.Store.DeleteLogsBefore(ctx, now.AddDate(0, 0, -s.LogRetentionDays))
	if err != nil {
		w.Log.Error("purge request logs", "err", err)
	} else if n > 0 {
		w.Log.Info("purged old request logs", "deleted", n, "retention_days", s.LogRetentionDays)
	}
}

// Purge is exported for tests and admin tooling.
func (w *Writer) Purge(ctx context.Context) { w.purge(ctx) }
