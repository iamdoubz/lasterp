// The well-behaved plugin: it does exactly what its manifest declares.
//
// It exists so the containment tests are not the only evidence — a sandbox
// that refuses everything passes every hostile test and is useless. This one
// proves the granted path works: echo, log, read an object, write an object,
// read a secret.
package main

import (
	"encoding/json"

	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/user lasterp_log
func hostLog(uint64) uint64

//go:wasmimport extism:host/user lasterp_object_get
func hostObjectGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_object_query
func hostObjectQuery(uint64) uint64

//go:wasmimport extism:host/user lasterp_object_create
func hostObjectCreate(uint64) uint64

//go:wasmimport extism:host/user lasterp_secret_get
func hostSecretGet(uint64) uint64

// call marshals req, hands it to a host function, and returns the raw JSON
// reply. Every host function has the same JSON-in/JSON-out shape.
func call(fn func(uint64) uint64, req any) []byte {
	body, err := json.Marshal(req)
	if err != nil {
		return []byte(`{"ok":false,"error":"marshal"}`)
	}
	arg := pdk.AllocateBytes(body)
	reply := pdk.FindMemory(fn(arg.Offset()))
	return reply.ReadBytes()
}

//go:wasmexport echo
func echo() int32 {
	pdk.OutputString("echo:" + pdk.InputString())
	return 0
}

//go:wasmexport say
func say() int32 {
	pdk.Output(call(hostLog, map[string]any{"message": pdk.InputString()}))
	return 0
}

// read takes {"object":..,"id":..} and returns the host's reply verbatim, so a
// test can see refusals as well as rows.
//
//go:wasmexport read
func read() int32 {
	var req map[string]any
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	if _, wantList := req["list"]; wantList {
		pdk.Output(call(hostObjectQuery, req))
		return 0
	}
	pdk.Output(call(hostObjectGet, req))
	return 0
}

//go:wasmexport write
func write() int32 {
	var req map[string]any
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(call(hostObjectCreate, req))
	return 0
}

//go:wasmexport secret
func secret() int32 {
	pdk.Output(call(hostSecretGet, map[string]any{"name": pdk.InputString()}))
	return 0
}

// chatter calls a host function far more often than any budget allows.
//
//go:wasmexport chatter
func chatter() int32 {
	for i := 0; i < 100000; i++ {
		call(hostLog, map[string]any{"message": "chatter"})
	}
	pdk.OutputString("finished")
	return 0
}

func main() {}
