// SPDX-License-Identifier: AGPL-3.0-only

// The hook plugin: the well-behaved half of WP-3.1b's corpus.
//
// It exercises every shape a sync hook can take — veto, enrich, pass — plus an
// async hook that records what it was delivered, and two hooks that fail on
// purpose so the containment tests have something that breaks in a *declared*
// way rather than by accident.
package main

import (
	"encoding/json"

	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/user lasterp_kv_get
func hostKVGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_kv_set
func hostKVSet(uint64) uint64

func call(fn func(uint64) uint64, req any) []byte {
	body, err := json.Marshal(req)
	if err != nil {
		return []byte(`{"ok":false}`)
	}
	arg := pdk.AllocateBytes(body)
	reply := pdk.FindMemory(fn(arg.Offset()))
	return reply.ReadBytes()
}

// hookRequest mirrors the host's sync hook payload.
type hookRequest struct {
	Object string         `json:"object"`
	Verb   string         `json:"verb"`
	Record map[string]any `json:"record"`
}

// asyncRequest mirrors the host's async delivery payload.
type asyncRequest struct {
	Object string `json:"object"`
	RefID  string `json:"ref_id"`
	Source string `json:"source"`
	Cursor int64  `json:"cursor"`
}

// veto refuses any record whose name contains "REJECT".
//
//go:wasmexport veto
func veto() int32 {
	var req hookRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	name, _ := req.Record["name"].(string)
	if contains(name, "REJECT") {
		return reply(map[string]any{"reject": "the name " + name + " is not allowed by com.acme.hooks"})
	}
	return reply(map[string]any{})
}

// enrich defaults a field the caller left out. The host re-validates whatever
// comes back, so this cannot be used to write something the schema forbids.
//
//go:wasmexport enrich
func enrich() int32 {
	var req hookRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	rec := req.Record
	if rec == nil {
		rec = map[string]any{}
	}
	if name, _ := rec["name"].(string); name != "" {
		rec["name"] = name + " (enriched)"
	}
	return reply(map[string]any{"record": rec})
}

// smuggle tries to enrich a record into a value the schema forbids — the check
// that a hook's output is re-validated rather than trusted (INV-T5).
//
//go:wasmexport smuggle
func smuggle() int32 {
	var req hookRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	rec := req.Record
	if rec == nil {
		rec = map[string]any{}
	}
	rec["kind"] = "not-a-declared-option"
	return reply(map[string]any{"record": rec})
}

// boom fails every time, for the fail-closed and breaker tests.
//
//go:wasmexport boom
func boom() int32 {
	pdk.SetErrorString("com.acme.hooks: deliberate failure")
	return 1
}

// note records each async delivery in plugin-scoped kv, so a test can see what
// was delivered and how often. This is also the idempotency pattern the host
// tells async authors to use, since delivery is at-least-once.
//
//go:wasmexport note
func note() int32 {
	var req asyncRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	key := "seen:" + req.Object + ":" + req.RefID
	existing := call(hostKVGet, map[string]any{"key": key})
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			Found bool   `json:"found"`
			Value string `json:"value"`
		} `json:"result"`
	}
	_ = json.Unmarshal(existing, &got)

	count := "1"
	if got.Result.Found {
		// Deliberately not parsed as a number: the test only needs to see that
		// a redelivery is visible, and string concatenation cannot overflow.
		count = got.Result.Value + "1"
	}
	call(hostKVSet, map[string]any{"key": key, "value": count})
	pdk.OutputString(count)
	return 0
}

// spawn writes a record of its own from inside an async hook, which is what
// would loop forever without self-suppression.
//
//go:wasmexport spawn
func spawn() int32 {
	var req asyncRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	out := call(hostObjectCreateFn, map[string]any{
		"object": req.Object,
		"record": map[string]any{"name": "spawned by com.acme.hooks", "kind": "customer"},
	})
	pdk.Output(out)
	return 0
}

//go:wasmimport extism:host/user lasterp_object_create
func hostObjectCreateFn(uint64) uint64

func reply(v map[string]any) int32 {
	body, err := json.Marshal(v)
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(body)
	return 0
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func main() {}
