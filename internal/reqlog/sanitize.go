// Package reqlog persists request records (request_logs): body sanitizing,
// asynchronous batched writes and retention.
package reqlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// minBase64Len is the minimum length of an unlabelled string treated as
	// base64 binary data (images) by the heuristic.
	minBase64Len = 512
	// maxNumberArray: longer arrays of numbers (embeddings, Ollama context
	// tokens) are summarized.
	maxNumberArray = 64
)

// Sanitize prepares a body for storage: base64 images become "[image: 12KB]"
// placeholders, long number arrays become "[N numbers]", and the result is
// truncated to maxBytes (0 = unlimited). Non-JSON bodies are only truncated.
// The original bytes are kept when nothing was replaced.
func Sanitize(body []byte, maxBytes int) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	out := body
	if json.Valid(body) {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) == nil {
			if nv, changed := walk(v, ""); changed {
				if b, err := json.Marshal(nv); err == nil {
					out = b
				}
			}
		}
	}
	return truncate(out, maxBytes)
}

func truncate(b []byte, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(b) <= maxBytes {
		return string(b), false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]) + fmt.Sprintf("\n…[truncated: original %d bytes]", len(b)), true
}

// walk replaces binary payloads. parentKey is the key under which v is stored.
func walk(v any, parentKey string) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		changed := false
		// Anthropic image source: {"type":"base64","media_type":...,"data":"..."}
		isB64Source := x["type"] == "base64"
		for k, val := range x {
			if s, ok := val.(string); ok && k == "data" && isB64Source && len(s) > 32 {
				x[k] = placeholder(s)
				changed = true
				continue
			}
			nv, c := walk(val, k)
			if c {
				x[k] = nv
				changed = true
			}
		}
		return x, changed
	case []any:
		if len(x) > maxNumberArray && allNumbers(x) {
			return fmt.Sprintf("[%d numbers]", len(x)), true
		}
		changed := false
		for i, val := range x {
			// Ollama: "images": ["<base64>", ...]
			if s, ok := val.(string); ok && parentKey == "images" && len(s) > 32 {
				x[i] = placeholder(s)
				changed = true
				continue
			}
			nv, c := walk(val, "")
			if c {
				x[i] = nv
				changed = true
			}
		}
		return x, changed
	case string:
		// OpenAI: "data:image/png;base64,...."
		if strings.HasPrefix(x, "data:") {
			if i := strings.Index(x, ";base64,"); i > 0 && len(x) > i+8+32 {
				return placeholder(x[i+8:]), true
			}
		}
		if len(x) >= minBase64Len && looksBase64(x) {
			return placeholder(x), true
		}
	}
	return v, false
}

func placeholder(b64 string) string {
	n := len(b64) * 3 / 4
	if n < 1024 {
		return fmt.Sprintf("[image: %dB]", n)
	}
	return fmt.Sprintf("[image: %dKB]", (n+512)/1024)
}

func allNumbers(a []any) bool {
	for _, v := range a {
		if _, ok := v.(json.Number); !ok {
			return false
		}
	}
	return true
}

func looksBase64(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/', c == '=', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
