// Package probe checks provider connectivity: it fetches the upstream model list
// and detects whether the provider serves the Anthropic Messages API.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"

	"llmgw/internal/store"
	"llmgw/internal/upstream"
)

// Result is the outcome of a connectivity check.
type Result struct {
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
	Models    []string `json:"models"`
	LatencyMS int64    `json:"latency_ms"`
	// Anthropic is the detected /v1/messages support (nil = not checked / unknown).
	Anthropic *bool `json:"anthropic_detected"`
}

// Check fetches the model list (Ollama /api/tags, OpenAI /v1/models) and,
// when detectAnthropic is set, probes /v1/messages.
func Check(ctx context.Context, client *http.Client, p *store.Provider, detectAnthropic bool) Result {
	start := time.Now()
	res := Result{Models: []string{}}
	path := "/models"
	if p.Type == store.ProviderOllama {
		path = "/api/tags"
	}
	b, err := upstream.Call(ctx, client, p, http.MethodGet, path, nil)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	var list struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		res.Error = "unexpected model list response: " + err.Error()
		return res
	}
	for _, m := range list.Models {
		res.Models = append(res.Models, m.Name)
	}
	for _, m := range list.Data {
		res.Models = append(res.Models, m.ID)
	}
	sort.Strings(res.Models)
	res.OK = true
	if detectAnthropic {
		res.Anthropic = detect(ctx, client, p)
	}
	return res
}

// detect sends an invalid Messages request. A missing route answers 404/405
// with a non-Anthropic body; an implementation answers 400/401/422 or a 404
// in Anthropic error format (e.g. "model not found"). The request uses an
// unknown model and no messages, so no model is loaded.
func detect(ctx context.Context, client *http.Client, p *store.Provider) *bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body := []byte(`{"model":"llmgw-probe-nonexistent","max_tokens":1,"messages":[]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.AnthropicURL(p, ""), bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if p.APIKey != "" {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	supported := true
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		var e struct {
			Type  string `json:"type"`
			Error *struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		supported = json.Unmarshal(rb, &e) == nil && e.Type == "error" && e.Error != nil
	}
	return &supported
}
