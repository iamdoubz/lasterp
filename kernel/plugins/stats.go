// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"sort"
	"sync"
	"time"
)

// Stats is per-plugin hook latency and outcome, kept so a slow plugin reads as
// *that plugin's* cost rather than as "the ERP is slow" — which is the
// incumbent failure docs/14 exists to avoid (WP-3.1b-decisions.md §7).
//
// Held per process in a bounded ring rather than in a table: these numbers
// answer "what is this plugin costing me", not "what happened last March", and
// a database write per hook call would be the plugin tax measuring itself. They
// reset on restart, and the API says so rather than implying history.
type Stats struct {
	mu sync.Mutex
	by map[string]*hookStat
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeFailed
	outcomeSkipped
)

// sampleWindow is how many recent durations a hook keeps for its percentile.
const sampleWindow = 256

type hookStat struct {
	Plugin  string
	Fn      string
	Calls   int64
	Failed  int64
	Skipped int64
	Total   time.Duration
	samples []time.Duration // ring, newest overwriting oldest
	next    int
}

// HookStat is one hook's measured cost, as reported to the API.
type HookStat struct {
	Plugin  string        `json:"plugin"`
	Fn      string        `json:"fn"`
	Calls   int64         `json:"calls"`
	Failed  int64         `json:"failed"`
	Skipped int64         `json:"skipped"`
	Mean    time.Duration `json:"mean_ms"`
	P95     time.Duration `json:"p95_ms"`
}

func NewStats() *Stats { return &Stats{by: map[string]*hookStat{}} }

// Record adds one invocation.
func (s *Stats) Record(plugin, fn string, d time.Duration, o outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := plugin + "." + fn
	st, ok := s.by[key]
	if !ok {
		st = &hookStat{Plugin: plugin, Fn: fn, samples: make([]time.Duration, 0, sampleWindow)}
		s.by[key] = st
	}
	switch o {
	case outcomeSkipped:
		st.Skipped++
		return
	case outcomeFailed:
		st.Failed++
	}
	st.Calls++
	st.Total += d
	if len(st.samples) < sampleWindow {
		st.samples = append(st.samples, d)
		return
	}
	st.samples[st.next] = d
	st.next = (st.next + 1) % sampleWindow
}

// For returns the recorded stats for one plugin, newest first by call count.
func (s *Stats) For(plugin string) []HookStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []HookStat
	for _, st := range s.by {
		if st.Plugin != plugin {
			continue
		}
		h := HookStat{Plugin: st.Plugin, Fn: st.Fn, Calls: st.Calls, Failed: st.Failed, Skipped: st.Skipped}
		if st.Calls > 0 {
			h.Mean = st.Total / time.Duration(st.Calls)
		}
		h.P95 = percentile(st.samples, 0.95)
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fn < out[j].Fn })
	return out
}

func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}
