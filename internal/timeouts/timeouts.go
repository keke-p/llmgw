// Package timeouts resolves effective per-request timeouts from the
// global settings -> provider -> alias hierarchy.
package timeouts

import (
	"time"

	"llmgw/internal/store"
)

// Source names where a value came from.
const (
	SourceGlobal   = "global"
	SourceProvider = "provider"
	SourceAlias    = "alias"
)

// Value is a duration with its origin. D == 0 means unlimited.
type Value struct {
	D      time.Duration
	Source string
}

// Effective is the resolved set of timeouts for one attempt.
type Effective struct {
	Connect        Value
	Queue          Value
	TTFT           Value
	Idle           Value
	MaxDuration    Value
	FallbackOnTTFT bool
	Keepalive      time.Duration
}

func pick(global time.Duration, layers ...layer) Value {
	v := Value{D: global, Source: SourceGlobal}
	for _, l := range layers {
		if l.d != nil {
			v = Value{D: *l.d, Source: l.source}
		}
	}
	return v
}

type layer struct {
	d      *time.Duration
	source string
}

// Resolve computes the effective timeouts. p or a may be nil.
func Resolve(s store.Settings, p *store.Provider, a *store.Alias) Effective {
	var pc, pq, pt, pi, pm, at, ai, am *time.Duration
	if p != nil {
		pc, pq, pt, pi, pm = p.ConnectTimeout, p.QueueTimeout, p.TTFTTimeout, p.IdleTimeout, p.MaxDuration
	}
	if a != nil {
		at, ai, am = a.TTFTTimeout, a.IdleTimeout, a.MaxDuration
	}
	e := Effective{
		Connect:        pick(s.ConnectTimeout, layer{pc, SourceProvider}),
		Queue:          pick(s.QueueTimeout, layer{pq, SourceProvider}),
		TTFT:           pick(s.TTFTTimeout, layer{pt, SourceProvider}, layer{at, SourceAlias}),
		Idle:           pick(s.IdleTimeout, layer{pi, SourceProvider}, layer{ai, SourceAlias}),
		MaxDuration:    pick(s.MaxDuration, layer{pm, SourceProvider}, layer{am, SourceAlias}),
		FallbackOnTTFT: s.FallbackOnTTFTTimeout,
		Keepalive:      s.KeepaliveInterval,
	}
	if a != nil && a.FallbackOnTTFTTimeout != nil {
		e.FallbackOnTTFT = *a.FallbackOnTTFTTimeout
	}
	return e
}
