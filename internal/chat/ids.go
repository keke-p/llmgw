package chat

import (
	"crypto/rand"
	"math/big"
)

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RandomString returns n random base62 characters.
func RandomString(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

// NewToolCallID generates an OpenAI-style tool call ID (used when upstream, e.g.
// Ollama, does not provide one).
func NewToolCallID() string { return "call_" + RandomString(24) }

// NewCompletionID generates an OpenAI-style completion ID.
func NewCompletionID() string { return "chatcmpl-" + RandomString(29) }
