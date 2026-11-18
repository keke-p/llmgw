// Package importer migrates LiteLLM Proxy configuration to llmgw: the
// config.yaml model_list (providers and aliases) and virtual keys exported from
// the LiteLLM database.
package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"llmgw/internal/store"
)

// Config is the subset of a LiteLLM config.yaml that is migrated.
type Config struct {
	ModelList []struct {
		ModelName     string         `yaml:"model_name"`
		LiteLLMParams map[string]any `yaml:"litellm_params"`
	} `yaml:"model_list"`
	LiteLLMSettings struct {
		Fallbacks []map[string][]string `yaml:"fallbacks"`
	} `yaml:"litellm_settings"`
	RouterSettings struct {
		Fallbacks []map[string][]string `yaml:"fallbacks"`
	} `yaml:"router_settings"`
}

// ParseConfig decodes a LiteLLM config.yaml.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if len(c.ModelList) == 0 {
		return nil, fmt.Errorf("config has no model_list entries")
	}
	return &c, nil
}

// PlanProvider is a provider to create or reuse.
type PlanProvider struct {
	Name       string
	Type       string
	BaseURL    string
	APIKey     string
	ExistingID int64 // > 0 when an identical provider already exists
}

// PlanTarget is one alias target.
type PlanTarget struct {
	Provider *PlanProvider
	Model    string
}

// PlanAlias is an alias to create.
type PlanAlias struct {
	Name     string
	Targets  []PlanTarget
	Defaults map[string]any
	Exists   bool // an alias with this name already exists
}

// Plan is the migration plan derived from a config.
type Plan struct {
	Providers []*PlanProvider
	Aliases   []*PlanAlias
	Warnings  []string
}

// paramNames are litellm_params carried over as alias "defaults".
var paramNames = map[string]bool{
	"temperature": true, "top_p": true, "top_k": true, "max_tokens": true, "stop": true, "seed": true,
	"presence_penalty": true, "frequency_penalty": true, "keep_alive": true,
	// Ollama options LiteLLM passes through
	"num_ctx": true, "num_predict": true, "repeat_penalty": true, "min_p": true, "mirostat": true,
	"num_gpu": true, "num_thread": true,
}

// ignoredParams are LiteLLM routing/accounting settings without an equivalent.
var ignoredParams = map[string]bool{
	"model": true, "api_base": true, "api_key": true, "rpm": true, "tpm": true, "timeout": true,
	"stream_timeout": true, "max_retries": true, "organization": true, "api_version": true,
	"input_cost_per_token": true, "output_cost_per_token": true, "drop_params": true, "order": true,
}

// splitModel parses "provider/model" into an llmgw provider type and model.
func splitModel(m string, apiBase string) (typ, model, defaultBase string, ok bool) {
	prefix, rest, hasPrefix := strings.Cut(m, "/")
	if !hasPrefix {
		prefix, rest = "", m
	}
	switch prefix {
	case "ollama", "ollama_chat":
		return store.ProviderOllama, rest, "http://localhost:11434", true
	case "lm_studio":
		return store.ProviderOpenAICompatible, rest, "http://localhost:1234/v1", true
	case "openai", "hosted_vllm", "openai_like", "text-completion-openai", "llamafile", "vllm":
		return store.ProviderOpenAICompatible, rest, "", true
	case "":
		if apiBase != "" { // an OpenAI-compatible server addressed by api_base
			return store.ProviderOpenAICompatible, rest, "", true
		}
	}
	return "", "", "", false
}

func providerName(typ, base string) string {
	short := map[string]string{store.ProviderOllama: "ollama", store.ProviderOpenAICompatible: "openai"}[typ]
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return short
	}
	return short + "-" + strings.ReplaceAll(u.Host, ":", "-")
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// BuildPlan converts a config into a plan. existing holds the current
// providers; aliasExists reports existing alias names. getenv resolves
// "os.environ/NAME" references.
func BuildPlan(c *Config, existing []*store.Provider, aliasExists func(string) bool, getenv func(string) string) *Plan {
	plan := &Plan{}
	byKey := map[string]*PlanProvider{}
	usedNames := map[string]bool{}
	for _, p := range existing {
		usedNames[p.Name] = true
	}
	aliases := map[string]*PlanAlias{}
	var order []string

	for i, m := range c.ModelList {
		where := fmt.Sprintf("model_list[%d] (%s)", i, m.ModelName)
		if m.ModelName == "" {
			plan.Warnings = append(plan.Warnings, where+": missing model_name; skipped")
			continue
		}
		lp := m.LiteLLMParams
		apiBase := strings.TrimRight(str(lp["api_base"]), "/")
		typ, model, defBase, ok := splitModel(str(lp["model"]), apiBase)
		if !ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: model %q is not a local/OpenAI-compatible provider; skipped", where, str(lp["model"])))
			continue
		}
		if apiBase == "" {
			apiBase = defBase
		}
		if apiBase == "" {
			plan.Warnings = append(plan.Warnings, where+": api_base is required for "+str(lp["model"])+"; skipped")
			continue
		}
		apiKey := str(lp["api_key"])
		if env, ok := strings.CutPrefix(apiKey, "os.environ/"); ok {
			apiKey = getenv(env)
			if apiKey == "" {
				plan.Warnings = append(plan.Warnings, where+": environment variable "+env+" is not set; provider API key left empty")
			}
		}
		key := typ + "|" + strings.ToLower(apiBase) + "|" + apiKey
		pp := byKey[key]
		if pp == nil {
			pp = &PlanProvider{Type: typ, BaseURL: apiBase, APIKey: apiKey}
			for _, e := range existing {
				if e.Type == typ && strings.EqualFold(strings.TrimRight(e.BaseURL, "/"), apiBase) && e.APIKey == apiKey {
					pp.ExistingID, pp.Name = e.ID, e.Name
					break
				}
			}
			if pp.ExistingID == 0 {
				base := providerName(typ, apiBase)
				name := base
				for n := 2; usedNames[name]; n++ {
					name = fmt.Sprintf("%s-%d", base, n)
				}
				usedNames[name] = true
				pp.Name = name
			}
			byKey[key] = pp
			plan.Providers = append(plan.Providers, pp)
		}
		a := aliases[m.ModelName]
		if a == nil {
			a = &PlanAlias{Name: m.ModelName, Defaults: map[string]any{}, Exists: aliasExists(m.ModelName)}
			aliases[m.ModelName] = a
			order = append(order, m.ModelName)
		}
		a.Targets = append(a.Targets, PlanTarget{Provider: pp, Model: model})
		var ignored []string
		for k, v := range lp {
			switch {
			case paramNames[k]:
				if _, set := a.Defaults[k]; !set {
					a.Defaults[k] = v
				}
			case !ignoredParams[k]:
				ignored = append(ignored, k)
			}
		}
		if len(ignored) > 0 {
			sort.Strings(ignored)
			plan.Warnings = append(plan.Warnings, where+": ignored litellm_params: "+strings.Join(ignored, ", "))
		}
	}

	// Fallbacks: append the targets of the fallback model groups, in order.
	for _, fbs := range [][]map[string][]string{c.RouterSettings.Fallbacks, c.LiteLLMSettings.Fallbacks} {
		for _, fb := range fbs {
			for name, to := range fb {
				a := aliases[name]
				if a == nil {
					continue
				}
				for _, other := range to {
					if o := aliases[other]; o != nil {
						for _, t := range o.Targets {
							if !hasTarget(a.Targets, t) {
								a.Targets = append(a.Targets, t)
							}
						}
					}
				}
			}
		}
	}
	for _, n := range order {
		plan.Aliases = append(plan.Aliases, aliases[n])
	}
	return plan
}

func hasTarget(ts []PlanTarget, t PlanTarget) bool {
	for _, x := range ts {
		if x.Provider == t.Provider && x.Model == t.Model {
			return true
		}
	}
	return false
}

// Summary describes what Apply did.
type Summary struct {
	ProvidersCreated, ProvidersReused int
	AliasesCreated, AliasesReplaced   int
	AliasesSkipped                    []string
}

// Apply writes the plan. Existing aliases are skipped unless replace is set.
func Apply(ctx context.Context, st *store.Store, plan *Plan, replace bool) (*Summary, error) {
	sum := &Summary{}
	for _, pp := range plan.Providers {
		if pp.ExistingID > 0 {
			sum.ProvidersReused++
			continue
		}
		p := &store.Provider{Name: pp.Name, Type: pp.Type, BaseURL: pp.BaseURL, APIKey: pp.APIKey, Enabled: true}
		if err := st.CreateProvider(ctx, p); err != nil {
			return sum, fmt.Errorf("create provider %s: %w", pp.Name, err)
		}
		pp.ExistingID = p.ID
		sum.ProvidersCreated++
	}
	for _, pa := range plan.Aliases {
		if pa.Exists && !replace {
			sum.AliasesSkipped = append(sum.AliasesSkipped, pa.Name)
			continue
		}
		var targets []store.Target
		for _, t := range pa.Targets {
			p, err := st.GetProvider(ctx, t.Provider.ExistingID)
			if err != nil {
				return sum, err
			}
			targets = append(targets, store.Target{Provider: p, UpstreamModel: t.Model})
		}
		params := json.RawMessage("{}")
		if len(pa.Defaults) > 0 {
			params, _ = json.Marshal(map[string]any{"defaults": pa.Defaults})
		}
		a := &store.Alias{Name: pa.Name, Enabled: true, Description: "imported from LiteLLM", Params: params, Targets: targets}
		if pa.Exists {
			cur, err := st.ResolveAliasAny(ctx, pa.Name)
			if err != nil {
				return sum, err
			}
			a.ID = cur.ID
			if err := st.UpdateAlias(ctx, a, true); err != nil {
				return sum, fmt.Errorf("replace alias %s: %w", pa.Name, err)
			}
			sum.AliasesReplaced++
			continue
		}
		if err := st.CreateAlias(ctx, a); err != nil {
			return sum, fmt.Errorf("create alias %s: %w", pa.Name, err)
		}
		sum.AliasesCreated++
	}
	return sum, nil
}
