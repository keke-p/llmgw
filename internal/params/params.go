// Package params applies an alias's parameter rules to requests:
//
//	{"defaults":  {...},   applied only when the client did not set the parameter
//	 "overrides": {...},   always applied ("max_tokens" acts as an upper bound)
//	 "system_prompt": {"mode": "prepend"|"replace", "text": "..."}}
//
// Parameter names use a common vocabulary: temperature, top_p, top_k, seed,
// max_tokens, stop, presence_penalty, frequency_penalty, keep_alive, think.
// Any other name is treated as an Ollama option (num_ctx, repeat_penalty,
// min_p, ...). Rules are applied to the common request on the conversion path
// and directly to the raw JSON on the passthrough path.
package params

import (
	"encoding/json"
	"strings"

	"llmgw/internal/chat"
)

// SystemPrompt is the alias system prompt rule.
type SystemPrompt struct {
	Mode string `json:"mode"` // prepend | replace
	Text string `json:"text"`
}

// Params are an alias's parameter rules.
type Params struct {
	Defaults     map[string]any `json:"defaults"`
	Overrides    map[string]any `json:"overrides"`
	SystemPrompt *SystemPrompt  `json:"system_prompt"`
}

// Parse decodes alias params (invalid or empty input yields nil).
func Parse(raw json.RawMessage) *Params {
	if len(raw) == 0 {
		return nil
	}
	var p Params
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	if len(p.Defaults) == 0 && len(p.Overrides) == 0 && (p.SystemPrompt == nil || p.SystemPrompt.Mode == "") {
		return nil
	}
	return &p
}

// known common parameter names; everything else is an Ollama option.
var known = map[string]bool{
	"temperature": true, "top_p": true, "top_k": true, "seed": true, "max_tokens": true, "stop": true,
	"presence_penalty": true, "frequency_penalty": true, "keep_alive": true, "think": true,
}

// anthropicCapable lists parameters an Anthropic Messages request can carry.
var anthropicCapable = map[string]bool{
	"temperature": true, "top_p": true, "top_k": true, "max_tokens": true, "stop": true, "think": true,
}

// PassthroughOK reports whether the rules can be applied to a raw request in
// the given format. If not (e.g. Ollama's num_ctx on an Anthropic request), the
// caller should convert instead of passing through.
func (p *Params) PassthroughOK(f chat.Format) bool {
	if p == nil || f != chat.FormatAnthropic {
		return true
	}
	for _, m := range []map[string]any{p.Defaults, p.Overrides} {
		for k := range m {
			if !anthropicCapable[k] {
				return false
			}
		}
	}
	return true
}

// ---- common request (conversion path) -------------------------------------

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func stopList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, s := range x {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	case []string:
		return x
	}
	return nil
}

func think(v any) *chat.Think {
	switch x := v.(type) {
	case bool:
		return &chat.Think{Enabled: x}
	case string:
		return &chat.Think{Enabled: true, Effort: x}
	}
	return nil
}

// isSet reports whether the client set parameter k in the common request.
func isSet(r *chat.Request, k string) bool {
	switch k {
	case "temperature":
		return r.Temperature != nil
	case "top_p":
		return r.TopP != nil
	case "top_k":
		return r.TopK != nil
	case "seed":
		return r.Seed != nil
	case "max_tokens":
		return r.MaxTokens != nil
	case "stop":
		return len(r.Stop) > 0
	case "presence_penalty":
		return r.PresencePenalty != nil
	case "frequency_penalty":
		return r.FrequencyPenalty != nil
	case "keep_alive":
		return len(r.KeepAlive) > 0
	case "think":
		return r.Think != nil && !r.Think.Implicit
	}
	_, ok := r.Options[k]
	return ok
}

func set(r *chat.Request, k string, v any, override bool) {
	switch k {
	case "temperature", "top_p", "presence_penalty", "frequency_penalty":
		f, ok := num(v)
		if !ok {
			return
		}
		switch k {
		case "temperature":
			r.Temperature = &f
		case "top_p":
			r.TopP = &f
		case "presence_penalty":
			r.PresencePenalty = &f
		case "frequency_penalty":
			r.FrequencyPenalty = &f
		}
	case "top_k":
		if f, ok := num(v); ok {
			n := int(f)
			r.TopK = &n
		}
	case "seed":
		if f, ok := num(v); ok {
			n := int64(f)
			r.Seed = &n
		}
	case "max_tokens":
		f, ok := num(v)
		if !ok {
			return
		}
		n := int(f)
		if override && r.MaxTokens != nil && *r.MaxTokens < n {
			return // upper bound: keep the smaller client value
		}
		r.MaxTokens = &n
	case "stop":
		r.Stop = stopList(v)
	case "keep_alive":
		r.KeepAlive, _ = json.Marshal(v)
	case "think":
		if t := think(v); t != nil {
			r.Think = t
		}
	default:
		if r.Options == nil {
			r.Options = map[string]any{}
		}
		r.Options[k] = v
	}
}

// ApplyChat applies the rules to a (cloned) common request.
func (p *Params) ApplyChat(r *chat.Request) {
	if p == nil {
		return
	}
	for k, v := range p.Defaults {
		if !isSet(r, k) {
			set(r, k, v, false)
		}
	}
	for k, v := range p.Overrides {
		set(r, k, v, true)
	}
	if sp := p.SystemPrompt; sp != nil && sp.Mode != "" {
		// Prompt-style requests (/v1/completions) carry it as a system message
		// too; the Ollama generate builder turns it into the "system" field.
		r.Messages = applySystem(r.Messages, sp)
	}
}

func applySystem(msgs []chat.Message, sp *SystemPrompt) []chat.Message {
	out := make([]chat.Message, 0, len(msgs)+1)
	if sp.Mode == "replace" {
		out = append(out, chat.Message{Role: "system", Content: []chat.Part{chat.TextPart(sp.Text)}})
		for _, m := range msgs {
			if m.Role != "system" {
				out = append(out, m)
			}
		}
		return out
	}
	if len(msgs) > 0 && msgs[0].Role == "system" {
		first := msgs[0]
		first.Content = []chat.Part{chat.TextPart(joinSystem(sp.Text, first.Text()))}
		out = append(out, first)
		return append(out, msgs[1:]...)
	}
	out = append(out, chat.Message{Role: "system", Content: []chat.Part{chat.TextPart(sp.Text)}})
	return append(out, msgs...)
}

func joinSystem(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" {
		return b
	}
	return a + "\n\n" + b
}

// ---- raw requests (passthrough path) --------------------------------------

// rawLocation describes where a parameter lives in a raw request.
type rawLocation struct {
	inOptions bool   // Ollama "options" object
	name      string // field name ("" = not representable)
}

func locate(f chat.Format, k string) rawLocation {
	switch f {
	case chat.FormatOllama:
		switch k {
		case "keep_alive", "think":
			return rawLocation{name: k}
		case "max_tokens":
			return rawLocation{inOptions: true, name: "num_predict"}
		}
		return rawLocation{inOptions: true, name: k}
	case chat.FormatAnthropic:
		switch k {
		case "temperature", "top_p", "top_k", "max_tokens":
			return rawLocation{name: k}
		case "stop":
			return rawLocation{name: "stop_sequences"}
		case "think":
			return rawLocation{name: "thinking"}
		}
		return rawLocation{}
	default: // OpenAI: everything top level
		if k == "think" {
			return rawLocation{name: "reasoning_effort"}
		}
		return rawLocation{name: k}
	}
}

// encode converts a common value into its raw representation.
func encode(f chat.Format, k string, v any) json.RawMessage {
	switch {
	case k == "stop" && f != chat.FormatOpenAI:
		b, _ := json.Marshal(stopList(v))
		return b
	case k == "think" && f == chat.FormatAnthropic:
		if t := think(v); t != nil && !t.Enabled {
			return json.RawMessage(`{"type":"disabled"}`)
		}
		return json.RawMessage(`{"type":"enabled","budget_tokens":4096}`)
	case k == "think" && f == chat.FormatOpenAI:
		if t := think(v); t != nil {
			if !t.Enabled {
				return json.RawMessage(`"none"`)
			}
			if t.Effort != "" {
				b, _ := json.Marshal(t.Effort)
				return b
			}
		}
		return json.RawMessage(`"medium"`)
	}
	b, _ := json.Marshal(v)
	return b
}

// ApplyRaw returns a copy of raw with the rules applied for the given wire
// format. kindGenerate selects Ollama /api/generate (system field instead of
// messages).
func (p *Params) ApplyRaw(raw map[string]json.RawMessage, f chat.Format, kindGenerate bool) map[string]json.RawMessage {
	if p == nil {
		return raw
	}
	out := make(map[string]json.RawMessage, len(raw)+4)
	for k, v := range raw {
		out[k] = v
	}
	var opts map[string]json.RawMessage
	if f == chat.FormatOllama {
		_ = json.Unmarshal(out["options"], &opts)
		if opts == nil {
			opts = map[string]json.RawMessage{}
		}
	}
	present := func(loc rawLocation) bool {
		if loc.inOptions {
			_, ok := opts[loc.name]
			return ok
		}
		if v, ok := out[loc.name]; ok && string(v) != "null" {
			return true
		}
		if f == chat.FormatOpenAI && loc.name == "max_tokens" {
			v, ok := out["max_completion_tokens"]
			return ok && string(v) != "null"
		}
		return false
	}
	put := func(loc rawLocation, v json.RawMessage) {
		if loc.inOptions {
			opts[loc.name] = v
		} else {
			out[loc.name] = v
		}
	}
	get := func(loc rawLocation) json.RawMessage {
		if loc.inOptions {
			return opts[loc.name]
		}
		return out[loc.name]
	}
	for k, v := range p.Defaults {
		loc := locate(f, k)
		if loc.name == "" || present(loc) {
			continue
		}
		put(loc, encode(f, k, v))
	}
	for k, v := range p.Overrides {
		loc := locate(f, k)
		if loc.name == "" {
			continue
		}
		if k == "max_tokens" {
			limit, ok := num(v)
			if !ok {
				continue
			}
			// upper bound on every max-tokens field the client may have used
			names := []string{loc.name}
			if f == chat.FormatOpenAI {
				names = append(names, "max_completion_tokens")
			}
			capped := false
			for _, n := range names {
				l := rawLocation{inOptions: loc.inOptions, name: n}
				var cur float64
				if b := get(l); b != nil && json.Unmarshal(b, &cur) == nil {
					if cur > limit {
						put(l, encode(f, k, v))
					}
					capped = true
				}
			}
			if !capped {
				put(loc, encode(f, k, v))
			}
			continue
		}
		put(loc, encode(f, k, v))
	}
	if f == chat.FormatOllama && len(opts) > 0 {
		out["options"], _ = json.Marshal(opts)
	}
	if sp := p.SystemPrompt; sp != nil && sp.Mode != "" {
		applyRawSystem(out, f, kindGenerate, sp)
	}
	return out
}

func applyRawSystem(out map[string]json.RawMessage, f chat.Format, generate bool, sp *SystemPrompt) {
	switch {
	case f == chat.FormatOllama && generate:
		var cur string
		_ = json.Unmarshal(out["system"], &cur)
		if sp.Mode == "prepend" {
			cur = joinSystem(sp.Text, cur)
		} else {
			cur = sp.Text
		}
		out["system"], _ = json.Marshal(cur)
	case f == chat.FormatAnthropic:
		var msgs []map[string]json.RawMessage
		_ = json.Unmarshal(out["messages"], &msgs)
		if sp.Mode == "replace" {
			out["system"], _ = json.Marshal(sp.Text)
			out["messages"], _ = json.Marshal(dropRoles(msgs, "system"))
			return
		}
		cur := out["system"]
		var s string
		var blocks []map[string]any
		switch {
		case len(cur) == 0 || string(cur) == "null":
			out["system"], _ = json.Marshal(sp.Text)
		case json.Unmarshal(cur, &s) == nil:
			out["system"], _ = json.Marshal(joinSystem(sp.Text, s))
		case json.Unmarshal(cur, &blocks) == nil:
			blocks = append([]map[string]any{{"type": "text", "text": sp.Text}}, blocks...)
			out["system"], _ = json.Marshal(blocks)
		}
	default: // OpenAI chat, Ollama chat: messages array
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(out["messages"], &msgs) != nil {
			return
		}
		sys := func(text string) map[string]json.RawMessage {
			c, _ := json.Marshal(text)
			return map[string]json.RawMessage{"role": json.RawMessage(`"system"`), "content": c}
		}
		if sp.Mode == "replace" {
			msgs = append([]map[string]json.RawMessage{sys(sp.Text)}, dropRoles(msgs, "system", "developer")...)
		} else if len(msgs) > 0 && isRole(msgs[0], "system", "developer") && prependContent(msgs[0], sp.Text) {
			// merged into the existing first system message
		} else {
			msgs = append([]map[string]json.RawMessage{sys(sp.Text)}, msgs...)
		}
		out["messages"], _ = json.Marshal(msgs)
	}
}

func isRole(m map[string]json.RawMessage, roles ...string) bool {
	var r string
	_ = json.Unmarshal(m["role"], &r)
	for _, x := range roles {
		if strings.EqualFold(r, x) {
			return true
		}
	}
	return false
}

func dropRoles(msgs []map[string]json.RawMessage, roles ...string) []map[string]json.RawMessage {
	out := msgs[:0:0]
	for _, m := range msgs {
		if !isRole(m, roles...) {
			out = append(out, m)
		}
	}
	return out
}

// prependContent merges text into a message whose content is a string or an
// array of parts. Returns false if the content has another shape.
func prependContent(m map[string]json.RawMessage, text string) bool {
	var s string
	if json.Unmarshal(m["content"], &s) == nil {
		m["content"], _ = json.Marshal(joinSystem(text, s))
		return true
	}
	var parts []map[string]any
	if json.Unmarshal(m["content"], &parts) == nil {
		parts = append([]map[string]any{{"type": "text", "text": text}}, parts...)
		m["content"], _ = json.Marshal(parts)
		return true
	}
	return false
}

// ---- embeddings ------------------------------------------------------------

// EmbedOptions returns keep_alive and Ollama options from the rules (sampling
// parameters do not apply to embeddings). defaults only fill missing keys.
func (p *Params) EmbedOptions(curOpts map[string]any, curKeepAlive json.RawMessage) (map[string]any, json.RawMessage) {
	if p == nil {
		return curOpts, curKeepAlive
	}
	opts := map[string]any{}
	for k, v := range curOpts {
		opts[k] = v
	}
	ka := curKeepAlive
	apply := func(m map[string]any, override bool) {
		for k, v := range m {
			switch {
			case k == "keep_alive":
				if override || len(ka) == 0 {
					ka, _ = json.Marshal(v)
				}
			case known[k]:
			default:
				if _, ok := opts[k]; override || !ok {
					opts[k] = v
				}
			}
		}
	}
	apply(p.Defaults, false)
	apply(p.Overrides, true)
	if len(opts) == 0 {
		opts = nil
	}
	return opts, ka
}
