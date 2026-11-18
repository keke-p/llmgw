package importer

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"llmgw/internal/auth"
	"llmgw/internal/clock"
	"llmgw/internal/config"
	"llmgw/internal/db"
	"llmgw/internal/keys"
	"llmgw/internal/store"
)

const sampleConfig = `
model_list:
  - model_name: qwen-coder
    litellm_params:
      model: ollama_chat/qwen3-coder:30b
      api_base: http://gpu-box:11434
      num_ctx: 32768
      rpm: 10
      custom_thing: 1
  - model_name: qwen-coder            # load-balanced second deployment
    litellm_params:
      model: ollama/qwen3-coder:30b
      api_base: http://gpu-box2:11434/
  - model_name: gemma
    litellm_params:
      model: lm_studio/gemma-3-27b
  - model_name: vllm-llama
    litellm_params:
      model: hosted_vllm/meta-llama/Llama-3.1-8B
      api_base: http://vllm:8000/v1
      api_key: os.environ/VLLM_KEY
      temperature: 0.2
  - model_name: cloud
    litellm_params:
      model: anthropic/claude-sonnet-4-5
  - model_name: claude-sonnet-4-5
    litellm_params:
      model: ollama_chat/qwen3-coder:30b
      api_base: http://gpu-box:11434
router_settings:
  fallbacks:
    - qwen-coder: ["gemma"]
`

func newStore(t *testing.T) *store.Store {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return store.New(d, config.DeriveKey("test"))
}

func TestBuildPlan(t *testing.T) {
	cfg, err := ParseConfig([]byte(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	existing := []*store.Provider{{ID: 9, Name: "my-lmstudio", Type: store.ProviderOpenAICompatible, BaseURL: "http://localhost:1234/v1"}}
	env := map[string]string{"VLLM_KEY": "secret"}
	plan := BuildPlan(cfg, existing, func(n string) bool { return n == "gemma" }, func(k string) string { return env[k] })

	names := map[string]*PlanProvider{}
	for _, p := range plan.Providers {
		names[p.Name] = p
	}
	if len(plan.Providers) != 4 {
		t.Fatalf("providers = %+v", plan.Providers)
	}
	if p := names["ollama-gpu-box-11434"]; p == nil || p.Type != store.ProviderOllama {
		t.Errorf("gpu-box provider: %+v", names)
	}
	if p := names["my-lmstudio"]; p == nil || p.ExistingID != 9 {
		t.Error("lm_studio default base must reuse the existing provider")
	}
	if p := names["openai-vllm-8000"]; p == nil || p.APIKey != "secret" {
		t.Errorf("os.environ api key: %+v", p)
	}
	aliases := map[string]*PlanAlias{}
	for _, a := range plan.Aliases {
		aliases[a.Name] = a
	}
	qc := aliases["qwen-coder"]
	if len(qc.Targets) != 3 || qc.Targets[0].Model != "qwen3-coder:30b" || qc.Targets[1].Provider.BaseURL != "http://gpu-box2:11434" ||
		qc.Targets[2].Model != "gemma-3-27b" {
		t.Errorf("qwen-coder targets (2 deployments + fallback) = %+v", qc.Targets)
	}
	if qc.Defaults["num_ctx"] != 32768 {
		t.Errorf("defaults = %v", qc.Defaults)
	}
	if !aliases["gemma"].Exists || aliases["cloud"] != nil {
		t.Error("exists flag / cloud provider skipped")
	}
	if aliases["vllm-llama"].Targets[0].Model != "meta-llama/Llama-3.1-8B" || aliases["vllm-llama"].Defaults["temperature"] != 0.2 {
		t.Errorf("vllm alias = %+v", aliases["vllm-llama"])
	}
	// the gpu-box provider is shared by two aliases
	if aliases["claude-sonnet-4-5"].Targets[0].Provider != qc.Targets[0].Provider {
		t.Error("identical providers must be deduplicated")
	}
	w := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(w, "custom_thing") || !strings.Contains(w, "anthropic/claude-sonnet-4-5") {
		t.Errorf("warnings = %s", w)
	}
}

func TestApply(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	cfg, _ := ParseConfig([]byte(sampleConfig))
	plan := BuildPlan(cfg, nil, func(string) bool { return false }, func(string) string { return "k" })
	sum, err := Apply(ctx, st, plan, false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ProvidersCreated != 4 || sum.AliasesCreated != 4 {
		t.Errorf("summary = %+v", sum)
	}
	a, err := st.ResolveAlias(ctx, "qwen-coder")
	if err != nil || len(a.Targets) != 3 || !strings.Contains(string(a.Params), `"num_ctx":32768`) {
		t.Fatalf("alias = %+v err=%v", a, err)
	}
	// Re-import: providers reused, aliases skipped (or replaced with -replace).
	existing, _ := st.ListProviders(ctx)
	exists := func(n string) bool { _, err := st.ResolveAliasAny(ctx, n); return err == nil }
	plan = BuildPlan(cfg, existing, exists, func(string) string { return "k" })
	sum, _ = Apply(ctx, st, plan, false)
	if sum.ProvidersReused != 4 || sum.ProvidersCreated != 0 || len(sum.AliasesSkipped) != 4 {
		t.Errorf("re-import = %+v", sum)
	}
	plan = BuildPlan(cfg, existing, exists, func(string) string { return "k" })
	if sum, _ = Apply(ctx, st, plan, true); sum.AliasesReplaced != 4 {
		t.Errorf("replace = %+v", sum)
	}
}

func TestImportLiteLLMKeys(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	litellmKey := "sk-AbCdEf0123456789"
	in := `{"token":"` + auth.SHA256Hex(litellmKey) + `","key_name":"sk-...6789","key_alias":"open-webui","models":[],"expires":null,"blocked":null,"rpm_limit":60,"max_parallel_requests":2}
{"token":"` + auth.SHA256Hex("sk-other") + `","key_name":"sk-...ther","key_alias":null,"models":["qwen-coder"],"expires":"2000-01-01T00:00:00","blocked":true,"rpm_limit":null,"max_parallel_requests":null}
{"token":"sk-plaintext-legacy","key_name":null,"key_alias":"legacy","models":["all-proxy-models"]}
{"token":"garbage"}
`
	ks, err := ReadKeys(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	res, err := ImportKeys(ctx, st, ks, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 3 || len(res.Warnings) != 1 {
		t.Errorf("result = %+v", res)
	}
	// The original LiteLLM key works unchanged.
	svc := keys.NewService(st, clock.Real{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, ge := svc.AuthenticateToken(ctx, litellmKey)
	if ge != nil || p.KeyName != "open-webui" || p.MaxConcurrency != 2 || len(p.AllowedAliases) != 0 {
		t.Fatalf("auth with LiteLLM key: p=%+v ge=%v", p, ge)
	}
	if _, ge := svc.AuthenticateToken(ctx, "sk-plaintext-legacy"); ge != nil {
		t.Errorf("legacy plaintext token: %v", ge)
	}
	if _, ge := svc.AuthenticateToken(ctx, "sk-other"); ge == nil {
		t.Error("blocked/expired key must not authenticate")
	}
	list, _ := st.ListKeys(ctx)
	for _, k := range list {
		if !k.Imported {
			t.Error("imported flag")
		}
		if k.Name == "sk-...ther" && (k.Status != store.KeyDisabled || k.AllowedAliases[0] != "qwen-coder" || k.ExpiresAt == nil) {
			t.Errorf("blocked key = %+v", k)
		}
	}
	// Re-import is idempotent.
	if res, _ := ImportKeys(ctx, st, ks, false); res.Imported != 0 || res.Duplicates != 3 {
		t.Errorf("re-import = %+v", res)
	}
	// JSON array input is accepted too.
	if arr, err := ReadKeys(strings.NewReader(`[{"token":"` + auth.SHA256Hex("x") + `"}]`)); err != nil || len(arr) != 1 {
		t.Errorf("array input: %v", err)
	}
}
