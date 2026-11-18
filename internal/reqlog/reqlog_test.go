package reqlog

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmgw/internal/clock"
	"llmgw/internal/db"
	"llmgw/internal/gateway"
	"llmgw/internal/store"
)

var b64 = strings.Repeat("QUJD", 1000) // 4000 chars = 3000 bytes

func TestSanitize(t *testing.T) {
	cases := []struct {
		name, in  string
		want      []string // substrings expected
		notWant   []string
		max       int
		truncated bool
		identical bool
	}{
		{name: "openai data url",
			in:   `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`,
			want: []string{`"url":"[image: 3KB]"`}, notWant: []string{"QUJD"}},
		{name: "ollama images",
			in:   `{"messages":[{"role":"user","content":"x","images":["` + b64 + `"]}]}`,
			want: []string{`"images":["[image: 3KB]"]`}},
		{name: "anthropic source",
			in:   `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}]}]}`,
			want: []string{`"data":"[image: 3KB]"`, `"media_type":"image/png"`}},
		{name: "embedding vectors",
			in:   `{"data":[{"embedding":[` + strings.TrimSuffix(strings.Repeat("0.125,", 100), ",") + `]}]}`,
			want: []string{`"embedding":"[100 numbers]"`}},
		{name: "small arrays and text untouched",
			in:        `{"b":[1,2,3],"a":"hello world","big":12345678901234567890}`,
			identical: true},
		{name: "truncation keeps utf8",
			in: `"` + strings.Repeat("日本語", 10) + `"`, max: 10, truncated: true,
			want: []string{"…[truncated: original 92 bytes]"}},
		{name: "non-json", in: "plain text body", identical: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, trunc := Sanitize([]byte(c.in), c.max)
			if trunc != c.truncated {
				t.Errorf("truncated = %v", trunc)
			}
			if c.identical && got != c.in {
				t.Errorf("must be kept verbatim:\n got %s\nwant %s", got, c.in)
			}
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %s in %s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %s", w)
				}
			}
			if c.truncated && !strings.HasPrefix(got, `"日本語`) {
				t.Errorf("bad cut: %q", got)
			}
		})
	}
}

func TestToRowLogModes(t *testing.T) {
	s := store.DefaultSettings()
	rec := &gateway.Record{CreatedAt: time.Now(), KeyID: 3, KeyName: "k", LogMode: store.LogFull,
		RequestBody: []byte(`{"q":1}`), ResponseBody: []byte(`{"a":1}`), UpstreamRequestBody: []byte(`{"u":1}`),
		Attempts: []gateway.Attempt{{ProviderName: "p", Result: "ok"}}, StatusCode: 200}
	r := ToRow(rec, s)
	if r.RequestBody == nil || *r.RequestBody != `{"q":1}` || r.ResponseBody == nil || r.UpstreamRequestBody != nil ||
		*r.KeyID != 3 || !strings.Contains(r.AttemptsJSON, `"provider":"p"`) {
		t.Errorf("full: %+v", r)
	}
	s.LogUpstreamRequest = true
	if r := ToRow(rec, s); r.UpstreamRequestBody == nil {
		t.Error("upstream request must be logged when enabled")
	}
	rec.LogMode = store.LogMetadataOnly
	if r := ToRow(rec, s); r.RequestBody != nil || r.ResponseBody != nil || r.UpstreamRequestBody != nil {
		t.Error("metadata_only must not store bodies")
	}
}

func newWriter(t *testing.T) (*Writer, *store.Store, *clock.Fake) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st := store.New(d, nil)
	c := clock.NewFake()
	return NewWriter(st, slog.New(slog.NewTextHandler(io.Discard, nil)), c), st, c
}

func TestWriterFlushAndModes(t *testing.T) {
	w, st, c := newWriter(t)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	for i := 0; i < 250; i++ {
		w.Record(&gateway.Record{CreatedAt: c.Now(), Endpoint: "/v1/chat/completions", InboundFormat: "openai",
			StatusCode: 200, LogMode: store.LogFull, RequestBody: []byte(`{"i":1}`)})
	}
	w.Record(&gateway.Record{CreatedAt: c.Now(), Endpoint: "/x", InboundFormat: "openai", LogMode: store.LogNone})
	cancel()
	w.Stop()
	w.Record(&gateway.Record{LogMode: store.LogFull}) // after Stop: ignored, no panic

	rows, err := st.ListLogs(context.Background(), store.LogFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 250 {
		t.Errorf("rows = %d (log_mode none must not be stored)", len(rows))
	}
}

func TestRetention(t *testing.T) {
	w, st, c := newWriter(t)
	ctx := context.Background()
	old := &store.LogRow{CreatedAt: c.Now().AddDate(0, 0, -31), Endpoint: "/a", InboundFormat: "openai", StatusCode: 200}
	recent := &store.LogRow{CreatedAt: c.Now().AddDate(0, 0, -1), Endpoint: "/b", InboundFormat: "openai", StatusCode: 200}
	if err := st.InsertLogs(ctx, []*store.LogRow{old, recent}); err != nil {
		t.Fatal(err)
	}
	w.Purge(ctx)
	rows, _ := st.ListLogs(ctx, store.LogFilter{})
	if len(rows) != 1 || rows[0].Endpoint != "/b" {
		t.Fatalf("after purge: %+v", rows)
	}
	st.SetSetting(ctx, "log_retention_days", 0) // 0 = keep forever
	c.Advance(365 * 24 * time.Hour)
	w.Purge(ctx)
	if rows, _ := st.ListLogs(ctx, store.LogFilter{}); len(rows) != 1 {
		t.Error("retention 0 must keep logs")
	}
}

func TestLogFilter(t *testing.T) {
	_, st, c := newWriter(t)
	ctx := context.Background()
	k := int64(7)
	body := func(s string) *string { return &s }
	st.InsertLogs(ctx, []*store.LogRow{
		{CreatedAt: c.Now(), KeyID: &k, Alias: "a", Endpoint: "/e", InboundFormat: "openai", StatusCode: 200, RequestBody: body(`find 100% me`)},
		{CreatedAt: c.Now(), Alias: "b", Endpoint: "/e", InboundFormat: "openai", StatusCode: 504, Error: "timeout"},
		{CreatedAt: c.Now().Add(-time.Hour), Alias: "a", Endpoint: "/e", InboundFormat: "openai", StatusCode: 404},
	})
	count := func(f store.LogFilter) int {
		rows, err := st.ListLogs(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	cases := map[string]struct {
		f    store.LogFilter
		want int
	}{
		"all":       {store.LogFilter{}, 3},
		"key":       {store.LogFilter{KeyID: &k}, 1},
		"alias":     {store.LogFilter{Alias: "a"}, 2},
		"5xx":       {store.LogFilter{Status: "5xx"}, 1},
		"error":     {store.LogFilter{Status: "error"}, 2},
		"exact":     {store.LogFilter{Status: "404"}, 1},
		"query":     {store.LogFilter{Query: "100% me"}, 1},
		"query esc": {store.LogFilter{Query: "1_0"}, 0},
		"from":      {store.LogFilter{From: c.Now().Add(-time.Minute)}, 2},
	}
	for name, cs := range cases {
		if got := count(cs.f); got != cs.want {
			t.Errorf("%s: %d want %d", name, got, cs.want)
		}
	}
	if err := store.ValidateLogFilter(store.LogFilter{Status: "abc"}); err == nil {
		t.Error("invalid status must error")
	}
}
