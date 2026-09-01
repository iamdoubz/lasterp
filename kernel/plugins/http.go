// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"

	"github.com/iamdoubz/lasterp/kernel/outbound"
)

// The outbound half of the sandbox (WP-3.2a, ADR-007, docs/05).
//
// WP-3.1a shipped no network at all, on the grounds that ADR-007 requires every
// outbound call to be **allowlisted and audited** and Extism's built-in client
// does the first and not the second. This is the audited client's plugin-shaped
// half, so the built-in one is still never enabled: `AllowedHosts` stays empty
// in runtime.go and every byte a plugin sends leaves through here.
//
// The transport itself lives in `kernel/outbound` (WP-3.3c), because an
// automation needs the same guard and there must only ever be one of it. What
// stays here is what makes a *plugin* a caller: the manifest is its allowlist
// and its principal is the audit row's actor.

// HTTPPolicy is the deployment's outbound posture, set by the operator for the
// whole host.
type HTTPPolicy = outbound.Policy

// Outbound bounds, re-exported for the plugin ABI's readers.
const (
	MaxHTTPRequestBytes  = outbound.MaxRequestBytes
	MaxHTTPResponseBytes = outbound.MaxResponseBytes
)

// ErrHTTPBlocked is every outbound refusal.
var ErrHTTPBlocked = outbound.ErrBlocked

// hostHTTPRequest is `lasterp_http_request`: the only way out of the sandbox.
//
//	{"method":"POST","url":"https://api.acme.com/x","headers":{...},"body":"..."}
//	→ {"status":200,"headers":{...},"body":"..."}
func hostHTTPRequest(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	body, _ := req["body"].(string)
	target, _ := req["url"].(string)

	resp, err := outbound.Do(ctx, inv.host.DB, inv.tenant, inv.host.HTTP,
		outbound.Caller{Object: "plugin", ID: inv.plugin.ID, Actor: inv.plugin.Principal()},
		// The manifest is the ceiling: an administrator approved "this plugin
		// talks to api.acme.com with GET and POST", and nothing else leaves.
		inv.plugin.Manifest.AllowsHTTP,
		outbound.Request{
			Method:  strOr(req["method"], "GET"),
			URL:     target,
			Headers: stringMap(req["headers"]),
			Body:    body,
		})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":  resp.Status,
		"headers": resp.Headers,
		"body":    string(resp.Body),
	}, nil
}

// stringMap narrows the plugin's JSON header object to strings. A non-string
// value is dropped rather than stringified: a header whose value is a number
// the plugin did not mean to send is a bug in the plugin, not a header.
func stringMap(raw any) map[string]string {
	in, _ := raw.(map[string]any)
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func strOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
