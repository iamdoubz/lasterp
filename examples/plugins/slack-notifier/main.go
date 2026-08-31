// SPDX-License-Identifier: Apache-2.0

// com.lasterp.slack-notifier — a worked example (WP-3.2b).
//
// It posts a line to a Slack incoming webhook when a Contact changes, which is
// the smallest honest example of a plugin that talks to the outside world:
//
//   - the webhook's secret path comes from the **vault**, never from the
//     manifest and never compiled into the module;
//   - the one host it may reach is **named in the manifest**, so an
//     administrator approving the install can see where their data goes, and
//     the sandbox refuses anything else — including a redirect to somewhere
//     else;
//   - every call is **audited** by the host, and the audit row carries the
//     destination but not the body, so this plugin's own credential does not
//     end up in the trail of the call that used it;
//   - and it dedupes, because async delivery is at-least-once and nobody wants
//     the same notification twice.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
package main

import (
	"encoding/json"

	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/user lasterp_log
func hostLog(uint64) uint64

//go:wasmimport extism:host/user lasterp_object_get
func hostObjectGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_secret_get
func hostSecretGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_http_request
func hostHTTPRequest(uint64) uint64

//go:wasmimport extism:host/user lasterp_kv_get
func hostKVGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_kv_set
func hostKVSet(uint64) uint64

type reply struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	Result json.RawMessage `json:"result"`
}

func call(fn func(uint64) uint64, req any) reply {
	body, err := json.Marshal(req)
	if err != nil {
		return reply{Error: "invalid"}
	}
	arg := pdk.AllocateBytes(body)
	out := pdk.FindMemory(fn(arg.Offset()))
	var r reply
	if err := json.Unmarshal(out.ReadBytes(), &r); err != nil {
		return reply{Error: "error"}
	}
	return r
}

func logf(msg string) { call(hostLog, map[string]any{"message": msg}) }

type asyncRequest struct {
	Object string `json:"object"`
	RefID  string `json:"ref_id"`
}

// on_contact_changed notifies Slack about one contact, once.
//
//go:wasmexport on_contact_changed
func on_contact_changed() int32 { //nolint:revive // the exported name is the manifest's
	var req asyncRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	dedupe := "notified:" + req.RefID
	if r := call(hostKVGet, map[string]any{"key": dedupe}); r.OK {
		var res struct {
			Found bool `json:"found"`
		}
		_ = json.Unmarshal(r.Result, &res)
		if res.Found {
			return 0
		}
	}

	got := call(hostObjectGet, map[string]any{"object": "Contact", "id": req.RefID})
	if !got.OK {
		logf("slack-notifier: cannot read contact " + req.RefID + ": " + got.Error)
		return 0
	}
	var contact struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(got.Result, &contact); err != nil {
		pdk.SetError(err)
		return 1
	}

	// The webhook URL is a credential — anyone holding it can post as you — so
	// it lives in the vault and never in this file. The *host* it points at is
	// public knowledge and lives in the manifest, where an administrator reads
	// it before approving; the sandbox refuses any other destination, so a
	// tampered secret cannot redirect this plugin somewhere else.
	secret := call(hostSecretGet, map[string]any{"name": "slack_webhook_url"})
	if !secret.OK {
		logf("slack-notifier: no webhook configured: " + secret.Error)
		return 0
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(secret.Result, &value); err != nil {
		pdk.SetError(err)
		return 1
	}

	message, err := json.Marshal(map[string]any{
		"text": "LastERP: contact " + contact.Name + " (" + contact.Kind + ") changed",
	})
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	posted := call(hostHTTPRequest, map[string]any{
		"method":  "POST",
		"url":     value.Value,
		"headers": map[string]any{"Content-Type": "application/json"},
		"body":    string(message),
	})
	if !posted.OK {
		// Left undeduped on purpose: a failed notification should be retried by
		// the delivery runner, and eventually dead-lettered where an
		// administrator can see it, rather than quietly marked as sent.
		logf("slack-notifier: webhook refused: " + posted.Error)
		return 1
	}
	call(hostKVSet, map[string]any{"key": dedupe, "value": "1"})
	return 0
}

func main() {}
