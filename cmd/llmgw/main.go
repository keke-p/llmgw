// Command llmgw is a lightweight gateway in front of local LLM providers
// (Ollama, LM Studio and other OpenAI-compatible servers).
//
// Usage:
//
//	llmgw [serve]          start the gateway (default)
//	llmgw healthcheck      probe /healthz of a running instance (for Docker HEALTHCHECK)
//	llmgw sql "<SQL>"      run SQL against the database (development aid)
//	llmgw backup <path>    online backup of the database
//	llmgw reset-admin-password [-username admin] [-password PW]
//	llmgw version          print the version
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"llmgw/internal/api/admin"
	"llmgw/internal/config"
	"llmgw/internal/db"
	"llmgw/internal/logging"
	"llmgw/internal/server"
	"llmgw/internal/store"
	"llmgw/internal/version"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "sql":
		err = runSQL(os.Args[2:])
	case "backup":
		err = backup(os.Args[2:])
	case "reset-admin-password":
		err = resetAdminPassword(os.Args[2:])
	case "import-litellm":
		err = importLiteLLM(os.Args[2:])
	case "import-litellm-keys":
		err = importLiteLLMKeys(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(version.Version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: llmgw <command>

Commands:
  serve        start the gateway (default)
  healthcheck  probe /healthz of a running instance
  sql "<SQL>"  run SQL against the database (reads stdin when no argument)
  backup <path>
               write an online backup of the database (safe while running)
  reset-admin-password [-username admin] [-password PW]
               set a new admin password (generated and printed when omitted)
  import-litellm [-dry-run] [-replace] <config.yaml>
               create providers and aliases from a LiteLLM config.yaml
  import-litellm-keys [-dry-run] <keys.jsonl>
               import virtual keys exported from the LiteLLM database
  version      print the version
`)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(os.Stdout, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()
	v, _ := db.SchemaVersion(ctx, database)
	log.Info("database ready", "path", cfg.DBPath, "schema_version", v)

	st := store.New(database, cfg.SecretKey)
	if err := admin.EnsureAdmin(ctx, st, cfg.AdminPassword, log); err != nil {
		return fmt.Errorf("create initial admin: %w", err)
	}
	if cfg.AdminTrustHeader != "" && len(cfg.TrustedProxies) == 0 {
		log.Warn("LLMGW_ADMIN_TRUST_HEADER is set but LLMGW_TRUSTED_PROXIES is empty; the header will be ignored")
	}

	return server.New(cfg, database, log).Run(ctx)
}

// healthcheck is used by the Docker HEALTHCHECK (distroless has no curl/wget).
func healthcheck() error {
	addr := os.Getenv("LLMGW_LISTEN")
	if addr == "" {
		addr = ":4000"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, strings.TrimSpace(resp.Status))
	}
	return nil
}
