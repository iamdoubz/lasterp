// SPDX-License-Identifier: AGPL-3.0-only

// The web plugin: WP-3.2a's corpus member for the two surfaces that WP added —
// outbound `lasterp_http_request` and inbound `/ext/<plugin>/` endpoints.
//
// It is both the well-behaved caller and the misbehaving one: `fetch` makes
// whatever request it is told to (so the tests can point it at a host its
// manifest never declared, at plaintext http, at a name that resolves to the
// metadata service), and `naughty` returns a response the server must refuse to
// pass through unchanged.
package main

import (
	"encoding/json"

	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/user lasterp_http_request
func hostHTTPRequest(uint64) uint64

//go:wasmimport extism:host/user lasterp_object_create
func hostObjectCreate(uint64) uint64

//go:wasmimport extism:host/user lasterp_secret_get
func hostSecretGet(uint64) uint64

func call(fn func(uint64) uint64, req any) []byte {
	body, err := json.Marshal(req)
	if err != nil {
		return []byte(`{"ok":false}`)
	}
	arg := pdk.AllocateBytes(body)
	reply := pdk.FindMemory(fn(arg.Offset()))
	return reply.ReadBytes()
}

// endpointRequest mirrors the host's inbound payload.
type endpointRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query"`
	Body   string `json:"body"`
	Caller string `json:"caller"`
}

// fetch makes the outbound request described by its input and hands back the
// host's raw reply, so a test sees exactly what the sandbox answered.
//
//go:wasmexport fetch
func fetch() int32 {
	var req map[string]any
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(call(hostHTTPRequest, req))
	return 0
}

// exfiltrate reads a secret it was granted and posts it to its allowlisted
// host — a legitimate plugin's whole reason for wanting both capabilities, and
// the case that proves the audit row does not become a copy of the credential.
//
//go:wasmexport exfiltrate
func exfiltrate() int32 {
	var req struct {
		Secret string `json:"secret"`
		URL    string `json:"url"`
	}
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	var got struct {
		OK     bool `json:"ok"`
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	_ = json.Unmarshal(call(hostSecretGet, map[string]any{"name": req.Secret}), &got)

	pdk.Output(call(hostHTTPRequest, map[string]any{
		"method":  "POST",
		"url":     req.URL,
		"headers": map[string]any{"Authorization": "Bearer " + got.Result.Value},
		"body":    `{"token":"` + got.Result.Value + `"}`,
	}))
	return 0
}

// report is a well-behaved endpoint: it echoes what the host told it about the
// call, which is how the tests check what an endpoint does and does not learn.
//
//go:wasmexport report
func report() int32 {
	var req endpointRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	body, err := json.Marshal(req)
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	return reply(map[string]any{
		"status":       200,
		"content_type": "application/json",
		"body":         string(body),
	})
}

// naughty returns everything an endpoint is not allowed to return: a redirect
// status, HTML on the host's own origin, and a body that would like to be a
// script.
//
//go:wasmexport naughty
func naughty() int32 {
	return reply(map[string]any{
		"status":       302,
		"content_type": "text/html",
		"body":         "<script>alert(document.cookie)</script>",
	})
}

// write creates a record from inside an endpoint, so a test can check whose
// authority the write ran under — the plugin's, never the caller's.
//
// The object and record come from the caller's body when it supplies them, so
// the same export works against the kernel tests' Widget and the product's
// Contact. That the *caller* names the object changes nothing: the manifest is
// still the ceiling, which is the property the app-level tests check.
//
//go:wasmexport write
func write() int32 {
	var req endpointRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	target := struct {
		Object string         `json:"object"`
		Record map[string]any `json:"record"`
	}{Object: "Widget", Record: map[string]any{"name": "made by com.acme.web", "kind": "customer"}}
	if req.Body != "" {
		_ = json.Unmarshal([]byte(req.Body), &target)
	}
	out := call(hostObjectCreate, map[string]any{
		"object": target.Object,
		"record": target.Record,
	})
	return reply(map[string]any{
		"status":       201,
		"content_type": "application/json",
		"body":         string(out),
	})
}

func reply(v map[string]any) int32 {
	body, err := json.Marshal(v)
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(body)
	return 0
}

func main() {}
