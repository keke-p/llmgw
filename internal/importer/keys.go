package importer

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"llmgw/internal/auth"
	"llmgw/internal/store"
)

// LiteLLMKey is one row of LiteLLM's "LiteLLM_VerificationToken" table,
// exported as JSON (see README for the psql command).
//
// LiteLLM stores virtual keys as hash_token(key) = sha256(key).hexdigest(),
// the same representation llmgw uses, so keys keep working without re-issue.
type LiteLLMKey struct {
	Token               string          `json:"token"`
	KeyName             *string         `json:"key_name"`  // display form, e.g. "sk-...abcd"
	KeyAlias            *string         `json:"key_alias"` // user-given name
	Models              json.RawMessage `json:"models"`
	Expires             *string         `json:"expires"`
	Blocked             *bool           `json:"blocked"`
	RPMLimit            *int            `json:"rpm_limit"`
	MaxParallelRequests *int            `json:"max_parallel_requests"`
}

// KeyResult summarizes a key import.
type KeyResult struct {
	Imported   int
	Duplicates int
	Warnings   []string
}

var timeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05"}

func parseTime(s string) (time.Time, error) {
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ToVirtualKey converts a LiteLLM row. Rows holding a plaintext key (very old
// LiteLLM versions) are hashed.
func (k *LiteLLMKey) ToVirtualKey() (*store.VirtualKey, error) {
	hash := strings.ToLower(strings.TrimSpace(k.Token))
	switch {
	case isSHA256Hex(hash):
	case strings.HasPrefix(k.Token, "sk-"):
		hash = auth.SHA256Hex(k.Token)
	default:
		return nil, errors.New("token is neither a SHA-256 hash nor a plaintext key")
	}
	v := &store.VirtualKey{KeyHash: hash, Status: store.KeyActive, LogMode: store.LogFull, Imported: true,
		AllowedAliases: []string{}}
	switch {
	case k.KeyAlias != nil && *k.KeyAlias != "":
		v.Name = *k.KeyAlias
	case k.KeyName != nil && *k.KeyName != "":
		v.Name = *k.KeyName
	default:
		v.Name = "litellm-" + hash[:8]
	}
	v.KeyPrefix = "litellm:" + hash[:8]
	if k.KeyName != nil && *k.KeyName != "" {
		v.KeyPrefix = *k.KeyName
	}
	var models []string
	if len(k.Models) > 0 && string(k.Models) != "null" {
		if err := json.Unmarshal(k.Models, &models); err != nil {
			return nil, fmt.Errorf("models: %w", err)
		}
	}
	for _, m := range models {
		if m == "all-proxy-models" || m == "*" {
			models = nil
			break
		}
	}
	if models != nil {
		v.AllowedAliases = models
	}
	if k.Expires != nil && *k.Expires != "" {
		t, err := parseTime(*k.Expires)
		if err != nil {
			return nil, err
		}
		v.ExpiresAt = &t
	}
	if k.Blocked != nil && *k.Blocked {
		v.Status = store.KeyDisabled
	}
	if k.RPMLimit != nil {
		v.RPMLimit = *k.RPMLimit
	}
	if k.MaxParallelRequests != nil {
		v.MaxConcurrency = *k.MaxParallelRequests
	}
	return v, nil
}

// ReadKeys parses JSON Lines (or a JSON array) of LiteLLM key rows.
func ReadKeys(r io.Reader) ([]LiteLLMKey, error) {
	br := bufio.NewReader(r)
	first, _ := br.Peek(1)
	if len(first) == 1 && first[0] == '[' {
		var all []LiteLLMKey
		if err := json.NewDecoder(br).Decode(&all); err != nil {
			return nil, err
		}
		return all, nil
	}
	var out []LiteLLMKey
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		var k LiteLLMKey
		if err := json.Unmarshal([]byte(t), &k); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, k)
	}
	return out, sc.Err()
}

// ImportKeys stores the keys; already present hashes are skipped.
func ImportKeys(ctx context.Context, st *store.Store, keys []LiteLLMKey, dryRun bool) (*KeyResult, error) {
	res := &KeyResult{}
	for i, k := range keys {
		v, err := k.ToVirtualKey()
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("row %d: %v; skipped", i+1, err))
			continue
		}
		if _, err := st.FindKeyByHash(ctx, v.KeyHash); err == nil {
			res.Duplicates++
			continue
		}
		if !dryRun {
			if err := st.CreateKey(ctx, v); err != nil {
				return res, fmt.Errorf("row %d: %w", i+1, err)
			}
		}
		res.Imported++
	}
	return res, nil
}
