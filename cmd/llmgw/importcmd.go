package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/db"
	"llmgw/internal/importer"
	"llmgw/internal/store"
)

// importLiteLLM migrates providers and aliases from a LiteLLM config.yaml.
func importLiteLLM(args []string) error {
	fs := flag.NewFlagSet("import-litellm", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan without writing")
	replace := fs.Bool("replace", false, "replace aliases that already exist (default: skip)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: llmgw import-litellm [-dry-run] [-replace] <config.yaml>")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	cfg, err := importer.ParseConfig(b)
	if err != nil {
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dbPath())
	if err != nil {
		return err
	}
	defer d.Close()
	var key []byte
	if s := os.Getenv("LLMGW_SECRET_KEY"); s != "" {
		key = config.DeriveKey(s)
	}
	st := store.New(d, key)
	existing, err := st.ListProviders(ctx)
	if err != nil {
		return err
	}
	plan := importer.BuildPlan(cfg, existing, func(name string) bool {
		_, err := st.ResolveAliasAny(ctx, name)
		return err == nil
	}, os.Getenv)

	fmt.Println("Providers:")
	for _, p := range plan.Providers {
		state := "create"
		if p.ExistingID > 0 {
			state = "reuse"
		}
		keyNote := ""
		if p.APIKey != "" {
			keyNote = " (api key)"
		}
		fmt.Printf("  [%s] %s  %s  %s%s\n", state, p.Name, p.Type, p.BaseURL, keyNote)
		if p.APIKey != "" && p.ExistingID == 0 && key == nil {
			return errors.New("LLMGW_SECRET_KEY is required to store provider API keys")
		}
	}
	fmt.Println("Aliases:")
	for _, a := range plan.Aliases {
		state := "create"
		if a.Exists {
			state = map[bool]string{true: "replace", false: "skip (exists)"}[*replace]
		}
		var ts []string
		for _, t := range a.Targets {
			ts = append(ts, t.Provider.Name+":"+t.Model)
		}
		extra := ""
		if len(a.Defaults) > 0 {
			extra = fmt.Sprintf("  defaults=%v", a.Defaults)
		}
		fmt.Printf("  [%s] %s -> %s%s\n", state, a.Name, strings.Join(ts, " , "), extra)
	}
	for _, w := range plan.Warnings {
		fmt.Println("warning:", w)
	}
	if *dryRun {
		fmt.Println("(dry run: nothing written)")
		return nil
	}
	sum, err := importer.Apply(ctx, st, plan, *replace)
	if err != nil {
		return err
	}
	_ = st.AddAudit(ctx, "cli", "import.litellm", "config", "", map[string]any{
		"providers_created": sum.ProvidersCreated, "aliases_created": sum.AliasesCreated,
		"aliases_replaced": sum.AliasesReplaced, "aliases_skipped": sum.AliasesSkipped}, "")
	fmt.Printf("done: providers created=%d reused=%d, aliases created=%d replaced=%d skipped=%d\n",
		sum.ProvidersCreated, sum.ProvidersReused, sum.AliasesCreated, sum.AliasesReplaced, len(sum.AliasesSkipped))
	return nil
}

// importLiteLLMKeys imports virtual keys exported from LiteLLM's database.
func importLiteLLMKeys(args []string) error {
	fs := flag.NewFlagSet("import-litellm-keys", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "validate without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: llmgw import-litellm-keys [-dry-run] <keys.jsonl>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	keys, err := importer.ReadKeys(f)
	if err != nil {
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, dbPath())
	if err != nil {
		return err
	}
	defer d.Close()
	st := store.New(d, nil)
	res, err := importer.ImportKeys(ctx, st, keys, *dryRun)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Println("warning:", w)
	}
	if !*dryRun {
		_ = st.AddAudit(ctx, "cli", "import.litellm_keys", "key", "", map[string]any{"imported": res.Imported, "duplicates": res.Duplicates}, "")
	}
	verb := "imported"
	if *dryRun {
		verb = "would import"
	}
	fmt.Printf("%s %d key(s); %d already present\n", verb, res.Imported, res.Duplicates)
	return nil
}
