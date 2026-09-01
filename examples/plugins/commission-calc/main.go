// SPDX-License-Identifier: Apache-2.0

// com.lasterp.commission-calc — a worked example (WP-3.2b).
//
// It answers one question: how much commission has each salesperson earned on
// posted invoices? The interesting part is not the arithmetic, it is the shape:
//
//   - it reacts **asynchronously**, because a commission total is not a reason
//     to make saving an invoice slower;
//   - delivery is **at-least-once**, so it dedupes on the invoice id rather
//     than assuming it is called exactly once;
//   - it keeps its own state in **plugin-scoped kv**, which nothing else can
//     read and which it cannot use to reach anything else;
//   - and it serves its answer on a route of its own under `/ext/`, so the
//     result is reachable through the same API as everything else.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
package main

import (
	"encoding/json"
	"strconv"

	"github.com/extism/go-pdk"
)

//go:wasmimport extism:host/user lasterp_log
func hostLog(uint64) uint64

//go:wasmimport extism:host/user lasterp_object_get
func hostObjectGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_kv_get
func hostKVGet(uint64) uint64

//go:wasmimport extism:host/user lasterp_kv_set
func hostKVSet(uint64) uint64

// commissionBasisPoints is 5% — a constant here because a rate is policy, and
// policy that varies per tenant belongs in a setting this plugin reads, not in
// a number an author edits and recompiles.
const commissionBasisPoints = 500

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

func kvGet(key string) (string, bool) {
	r := call(hostKVGet, map[string]any{"key": key})
	if !r.OK {
		return "", false
	}
	var res struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	_ = json.Unmarshal(r.Result, &res)
	return res.Value, res.Found
}

func kvSet(key, value string) { call(hostKVSet, map[string]any{"key": key, "value": value}) }

// asyncRequest is what the delivery runner hands an async hook. There is no
// verb on a change-feed entry — it says an object changed, not what was done to
// it — so the hook reads current state and decides.
type asyncRequest struct {
	Object string `json:"object"`
	RefID  string `json:"ref_id"`
}

// endpointRequest is what an /ext route hands an endpoint function.
type endpointRequest struct {
	Query  string `json:"query"`
	Caller string `json:"caller"`
}

// on_invoice_changed accrues commission for one invoice, once.
//
//go:wasmexport on_invoice_changed
func on_invoice_changed() int32 { //nolint:revive // the exported name is the manifest's
	var req asyncRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}

	// At-least-once means this function must be safe to run twice on the same
	// invoice. The dedupe key is the invoice id, and it is written *after* the
	// total, so a crash between the two costs a duplicate rather than a loss —
	// the direction to fail in when the alternative is silently missing money.
	if _, seen := kvGet("counted:" + req.RefID); seen {
		return 0
	}

	got := call(hostObjectGet, map[string]any{"object": "Invoice", "id": req.RefID})
	if !got.OK {
		// A refusal is not a crash: the invoice may have been deleted between
		// the change and this delivery, and a plugin that traps on that would
		// trip its own circuit breaker for a race it cannot avoid.
		logf("commission-calc: cannot read invoice " + req.RefID + ": " + got.Error)
		return 0
	}
	var invoice struct {
		Status    string  `json:"status"`
		Currency  string  `json:"currency"`
		NetMinor  float64 `json:"net_minor"`
		ContactID string  `json:"contact_id"`
	}
	if err := json.Unmarshal(got.Result, &invoice); err != nil {
		pdk.SetError(err)
		return 1
	}
	// Only posted invoices earn commission. A draft that is later posted comes
	// back through this same hook, because posting is a change.
	if invoice.Status != "posted" {
		return 0
	}

	// Integer minor units throughout — the same rule the ledger keeps. A
	// float here would be a rounding error someone is paid.
	commission := int64(invoice.NetMinor) * commissionBasisPoints / 10000
	key := "total:" + invoice.Currency
	running, _ := kvGet(key)
	previous, _ := strconv.ParseInt(running, 10, 64)
	kvSet(key, strconv.FormatInt(previous+commission, 10))
	kvSet("counted:"+req.RefID, "1")
	return 0
}

// report serves GET /ext/com.lasterp.commission-calc/report?currency=EUR.
//
//go:wasmexport report
func report() int32 {
	var req endpointRequest
	if err := pdk.InputJSON(&req); err != nil {
		pdk.SetError(err)
		return 1
	}
	currency := "EUR"
	if value, ok := queryValue(req.Query, "currency"); ok {
		currency = value
	}
	total, _ := kvGet("total:" + currency)
	if total == "" {
		total = "0"
	}
	body, err := json.Marshal(map[string]any{
		"currency":         currency,
		"commission_minor": json.RawMessage(total),
		"basis_points":     commissionBasisPoints,
		"requested_by":     req.Caller,
	})
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	out, err := json.Marshal(map[string]any{
		"status": 200, "content_type": "application/json", "body": string(body),
	})
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	pdk.Output(out)
	return 0
}

// queryValue is a tiny query-string reader: the sandbox has no net/url, and
// pulling one parameter out does not need it.
func queryValue(query, want string) (string, bool) {
	for len(query) > 0 {
		pair := query
		if i := indexByte(query, '&'); i >= 0 {
			pair, query = query[:i], query[i+1:]
		} else {
			query = ""
		}
		if i := indexByte(pair, '='); i > 0 && pair[:i] == want {
			return pair[i+1:], true
		}
	}
	return "", false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func main() {}
