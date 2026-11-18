// Package health periodically checks provider connectivity and records the
// result (shown on the admin dashboard).
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"llmgw/internal/clock"
	"llmgw/internal/probe"
	"llmgw/internal/store"
)

const (
	checkTimeout = 10 * time.Second
	minInterval  = 5 * time.Second
	idleRecheck  = time.Minute // when checks are disabled (interval 0)
)

// Checker runs provider health checks.
type Checker struct {
	Store  *store.Store
	Client *http.Client
	Clock  clock.Clock
	Log    *slog.Logger
}

// Run checks all enabled providers every health_check_interval (re-read from
// settings each round, so changes apply without restart) until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	for {
		wait := idleRecheck
		if s, err := c.Store.Settings(ctx); err == nil && s.HealthCheckInterval > 0 {
			c.CheckAll(ctx)
			wait = max(s.HealthCheckInterval, minInterval)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// CheckAll checks every enabled provider concurrently.
func (c *Checker) CheckAll(ctx context.Context) {
	ps, err := c.Store.ListProviders(ctx)
	if err != nil {
		c.Log.Warn("health check: list providers", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, p := range ps {
		if !p.Enabled {
			continue
		}
		wg.Add(1)
		go func(p *store.Provider) {
			defer wg.Done()
			c.Check(ctx, p)
		}(p)
	}
	wg.Wait()
}

// Check probes one provider and stores the result. A status change is logged.
func (c *Checker) Check(ctx context.Context, p *store.Provider) probe.Result {
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	res := probe.Check(cctx, c.Client, p, false)
	if ctx.Err() != nil {
		return res // shutting down: do not record a spurious failure
	}
	status, models := "error", ""
	if res.OK {
		status = "ok"
		b, _ := json.Marshal(res.Models)
		models = string(b)
	}
	if status != p.HealthStatus {
		if res.OK {
			c.Log.Info("provider healthy", "provider", p.Name, "latency_ms", res.LatencyMS)
		} else {
			c.Log.Warn("provider unhealthy", "provider", p.Name, "err", res.Error)
		}
	}
	if err := c.Store.SetProviderHealth(context.WithoutCancel(ctx), p.ID, status, res.Error, models, c.Clock.Now()); err != nil {
		c.Log.Warn("health check: store result", "err", err)
	}
	return res
}
