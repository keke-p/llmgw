package timeouts

import (
	"testing"
	"time"

	"llmgw/internal/store"
)

func dur(d time.Duration) *time.Duration { return &d }

func TestResolve(t *testing.T) {
	s := store.DefaultSettings()
	tru := true
	cases := []struct {
		name     string
		p        *store.Provider
		a        *store.Alias
		wantTTFT Value
		wantConn Value
		wantFB   bool
	}{
		{"global only", nil, nil, Value{300 * time.Second, SourceGlobal}, Value{10 * time.Second, SourceGlobal}, false},
		{"provider overrides", &store.Provider{TTFTTimeout: dur(60 * time.Second), ConnectTimeout: dur(time.Second)}, &store.Alias{},
			Value{60 * time.Second, SourceProvider}, Value{time.Second, SourceProvider}, false},
		{"alias wins", &store.Provider{TTFTTimeout: dur(60 * time.Second)}, &store.Alias{TTFTTimeout: dur(0), FallbackOnTTFTTimeout: &tru},
			Value{0, SourceAlias}, Value{10 * time.Second, SourceGlobal}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Resolve(s, c.p, c.a)
			if e.TTFT != c.wantTTFT || e.Connect != c.wantConn || e.FallbackOnTTFT != c.wantFB {
				t.Errorf("got ttft=%v conn=%v fb=%v", e.TTFT, e.Connect, e.FallbackOnTTFT)
			}
		})
	}
}
