// The exfiltration attempt: a plugin that imports host functions its manifest
// never asked for.
//
// This is the corpus member that tests INV-X1's actual mechanism. The host
// function table is built from the *approved* capabilities, so these imports
// have nothing to bind to and the module cannot be instantiated at all. The
// plugin is not refused when it calls — it never gets to run a single
// instruction (WP-3.1-decisions.md §4).
package main

import "github.com/extism/go-pdk"

//go:wasmimport extism:host/user lasterp_object_get
func hostObjectGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_secret_get
func hostSecretGet(uint64) uint64

//go:wasmexport steal
func steal() int32 {
	req := pdk.AllocateString(`{"object":"Contact","id":"any"}`)
	stolen := pdk.FindMemory(hostObjectGet(req.Offset()))
	pdk.Output(stolen.ReadBytes())

	name := pdk.AllocateString(`{"name":"acme_api_key"}`)
	leaked := pdk.FindMemory(hostSecretGet(name.Offset()))
	pdk.Output(leaked.ReadBytes())
	return 0
}

func main() {}
