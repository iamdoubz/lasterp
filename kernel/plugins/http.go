// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// The outbound half of the sandbox (WP-3.2a, ADR-007, docs/05).
//
// WP-3.1a shipped no network at all, on the grounds that ADR-007 requires every
// outbound call to be **allowlisted and audited** and Extism's built-in client
// does the first and not the second. This is the audited client, so the
// built-in one is still never enabled: `AllowedHosts` stays empty in
// runtime.go and every byte a plugin sends leaves through here.
//
// The allowlist is a manifest declaration an administrator approved, but a
// declaration is a name and a name is not a destination — so the checks that
// matter run on the address actually dialled (dialGuard) rather than on the
// string in the YAML.

// HTTPPolicy is the deployment's outbound posture. It is set by the operator
// for the whole host, never by a plugin: a plugin cannot ask to reach the
// internal network, an operator can decide that plugins may.
type HTTPPolicy struct {
	// AllowPrivateNetworks permits loopback, RFC1918, link-local and the other
	// non-public ranges. Off by default, because the default deployment's
	// private network holds the database, the metadata service of whatever
	// cloud it runs in, and every other service that trusts its own LAN.
	//
	// A self-hoster whose plugin calls an internal service turns it on
	// knowingly (docs/09 §Plugin outbound HTTP).
	AllowPrivateNetworks bool
	// RootCAs replaces the system trust store, for the deployment whose
	// internal service presents a private CA's certificate. nil means the
	// system roots, which is what a public destination needs.
	RootCAs *x509.CertPool
}

// Outbound bounds. Small on purpose: a plugin is an extension point, not a
// proxy, and a 1MB reply is already more JSON than a hook should be reading
// inside a write path.
const (
	MaxHTTPRequestBytes  = 256 << 10
	MaxHTTPResponseBytes = 1 << 20
	maxResponseHeaders   = 64
	dialTimeout          = 5 * time.Second
)

// ErrHTTPBlocked is every outbound refusal: a host the manifest did not
// declare, a scheme that is not https, a redirect, an address that is not
// public. One error because the plugin is told "no", not which rule said so —
// the classify() rule from WP-3.1a applied to the network.
var ErrHTTPBlocked = errors.New("plugins: outbound request blocked")

// hostHTTPRequest is `lasterp_http_request`: the only way out of the sandbox.
//
//	{"method":"POST","url":"https://api.acme.com/x","headers":{...},"body":"..."}
//	→ {"status":200,"headers":{...},"body":"..."}
func hostHTTPRequest(ctx context.Context, inv *invocation, req map[string]any) (any, error) {
	method := strings.ToUpper(strOr(req["method"], "GET"))
	target, _ := req["url"].(string)

	u, hostPort, err := outboundTarget(target)
	if err != nil {
		return nil, err
	}
	// The manifest is the ceiling, checked before anything is dialled or
	// logged. An administrator approved "this plugin talks to api.acme.com
	// with GET and POST"; nothing else leaves.
	if !inv.plugin.Manifest.AllowsHTTP(method, hostPort) {
		return nil, fmt.Errorf("%w: %s %s is not in this plugin's http allowlist", ErrHTTPBlocked, method, hostPort)
	}

	body, _ := req["body"].(string)
	if len(body) > MaxHTTPRequestBytes {
		return nil, fmt.Errorf("%w: request body is %d bytes, over the %d-byte limit", ErrHTTPBlocked, len(body), MaxHTTPRequestBytes)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHTTPBlocked, err)
	}
	setPluginHeaders(httpReq, req["headers"])

	// **Audited before it is dialled.** ADR-007 requires every outbound call to
	// be audited; recording the intent before the socket opens is the only
	// ordering in which no call can happen without a row. `audit_log` is
	// append-only (docs/19 §2), so this row cannot be amended with the status
	// afterwards — the plugin gets the status, the operator gets the fact of
	// the call. The URL's query string is deliberately not recorded: API keys
	// live there.
	//
	// ponytail: one row per call, request-side only. A second completion row
	// with the status is the upgrade if operators need outcomes, and it doubles
	// the volume of the noisiest audit action.
	//
	// The path is recorded to its first segment only — see auditablePath.
	if err := auditOutbound(ctx, inv, method, hostPort, auditablePath(u.Path)); err != nil {
		return nil, err
	}

	resp, err := httpClient(inv.host.HTTP).Do(httpReq)
	if err != nil {
		// Wrapped, not returned raw: a dial error carries the resolved address
		// and the plugin does not need to learn the shape of the network it was
		// refused from.
		return nil, fmt.Errorf("%w: %s", ErrHTTPBlocked, classifyDialError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxHTTPResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response failed", ErrHTTPBlocked)
	}
	if len(respBody) > MaxHTTPResponseBytes {
		return nil, fmt.Errorf("%w: response is over the %d-byte limit", ErrHTTPBlocked, MaxHTTPResponseBytes)
	}

	return map[string]any{
		"status":  resp.StatusCode,
		"headers": responseHeaders(resp.Header),
		"body":    string(respBody),
	}, nil
}

// outboundTarget parses and bounds the destination, returning the URL and its
// host with the port made explicit so the allowlist compares like with like.
func outboundTarget(target string) (*url.URL, string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %q is not a URL", ErrHTTPBlocked, target)
	}
	// https only. Plaintext outbound from a server holding tenant data is not
	// a knob, and it is the difference between "this plugin talks to
	// api.acme.com" and "this plugin talks to whoever is on the path".
	if u.Scheme != "https" {
		return nil, "", fmt.Errorf("%w: scheme %q is not https", ErrHTTPBlocked, u.Scheme)
	}
	if u.User != nil {
		return nil, "", fmt.Errorf("%w: credentials in the URL are not accepted", ErrHTTPBlocked)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, "", fmt.Errorf("%w: no host", ErrHTTPBlocked)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u, host + ":" + port, nil
}

// setPluginHeaders copies the plugin's headers onto the request. Hop-by-hop
// and identity-bearing headers are dropped: Host would make the allowlisted
// destination a lie, and the rest are the transport's business.
func setPluginHeaders(r *http.Request, raw any) {
	headers, _ := raw.(map[string]any)
	for k, v := range headers {
		value, ok := v.(string)
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "host", "content-length", "connection", "transfer-encoding", "upgrade":
			continue
		}
		if !validHeaderName(k) || strings.ContainsAny(value, "\r\n") {
			continue
		}
		r.Header.Set(k, value)
	}
}

func validHeaderName(k string) bool {
	if k == "" || len(k) > 128 {
		return false
	}
	for _, c := range k {
		if c != '-' && c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// responseHeaders renders the reply's headers for the plugin, capped.
func responseHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(out) >= maxResponseHeaders {
			break
		}
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// auditOutbound writes the INV-T4 row: which plugin called out, where, and
// when. No headers and no bodies — a plugin that reads `acme_api_key` from the
// vault and sends it as a bearer token would otherwise write that secret into
// the audit log in the clear, which is exactly what INV-K1 forbids. This row
// says a call happened; it is not a proxy log.
func auditOutbound(ctx context.Context, inv *invocation, method, hostPort, path string) error {
	err := tenancy.WithTenant(ctx, inv.host.DB, inv.tenant, func(ctx context.Context, tx *sql.Tx) error {
		return recordAudit(ctx, tx, inv.host.DB, inv.tenant, inv.plugin.ID, "http.request", inv.plugin.Principal(),
			map[string]any{"method": method, "host": hostPort, "path": path})
	})
	if err != nil {
		// Unaudited calls do not happen: if the row cannot be written, the
		// request is not made. ADR-007 says audited, and "we tried" is not
		// audited.
		return fmt.Errorf("%w: the call could not be audited: %w", ErrHTTPBlocked, err)
	}
	return nil
}

// httpClient returns the shared client for a policy. One exists per posture for
// the life of the process, so connections pool and the guard below is installed
// exactly once — a deployment has one policy, so in production this map holds
// one entry.
var (
	clientsMu sync.Mutex
	clients   = map[HTTPPolicy]*http.Client{}
)

func httpClient(p HTTPPolicy) *http.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clients[p]; ok {
		return c
	}
	c := newHTTPClient(p)
	clients[p] = c
	return c
}

func newHTTPClient(p HTTPPolicy) *http.Client {
	dialer := &net.Dialer{Timeout: dialTimeout, Control: dialGuard(p.AllowPrivateNetworks)}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			// nil RootCAs is the system trust store, which is what https-only
			// is for; a deployment with an internal CA supplies its own.
			TLSClientConfig: &tls.Config{RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12},
		},
		// **Redirects are not followed.** A 302 to a host the manifest never
		// declared is how an allowlist is bypassed in one hop, and following it
		// would re-run the guard on an address the administrator never
		// approved. The 3xx is handed back to the plugin, which may re-request
		// inside its own allowlist.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dialGuard refuses to open a socket to a non-public address.
//
// It runs in Dialer.Control, which fires **after** resolution with the address
// the kernel is about to connect to, so an allowlisted name that resolves to
// 169.254.169.254 — the cloud metadata service, the classic SSRF prize — is
// refused on the address rather than trusted on the name. That placement is
// also what makes DNS rebinding pointless: there is no window between the
// check and the connection for a second answer to land in.
func dialGuard(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrHTTPBlocked, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !isPublicIP(ip) {
			return fmt.Errorf("%w: %s is not a public address", ErrHTTPBlocked, host)
		}
		return nil
	}
}

// blockedNets are the ranges Go's own predicates do not cover: carrier-grade
// NAT, the IETF protocol block (which holds 192.0.0.170), the benchmarking
// range, and the reserved class E space.
var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{
		"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
	} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// nat64Prefix is the well-known translation prefix: 64:ff9b::/96 carries an
// IPv4 address in its low 32 bits, so a NAT64 network reaches 127.0.0.1 as
// 64:ff9b::7f00:1 and every IPv4 rule above would miss it.
var nat64Prefix = net.IPNet{IP: net.ParseIP("64:ff9b::"), Mask: net.CIDRMask(96, 128)}

func isPublicIP(ip net.IP) bool {
	if nat64Prefix.Contains(ip) {
		ip = ip[12:16] // the embedded IPv4 address, judged on its own merits
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// IsGlobalUnicast is false for loopback, link-local, multicast and the
	// unspecified address in one call; IsPrivate covers RFC1918 and IPv6 ULA.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// classifyDialError keeps server network topology out of a plugin's error
// string while still telling it apart from a refusal it can fix.
func classifyDialError(err error) string {
	if errors.Is(err, ErrHTTPBlocked) {
		return "the destination address is not permitted"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the request did not finish inside this call's budget"
	}
	return "the request failed"
}

// auditablePath is the part of a URL path safe to write down: the first
// segment, and nothing after it.
//
// The full path cannot be recorded, because for a whole class of APIs **the
// path is the credential** — a Slack incoming webhook is
// `/services/T000/B000/<secret>`, and so is every "unguessable URL" integration
// built the same way. Writing that into `audit_log` would put a live credential
// in the trail of the call that used it, which is precisely INV-K1, and the
// plugin holding it did nothing wrong: it was granted `secrets:` and `http:`
// and used both as intended.
//
// The first segment is kept because it is what makes a row *useful* to an
// operator — `/services` and `/admin` on the same host are different answers to
// "what is this plugin doing" — and because a first segment is a route family,
// not an identifier. The query string is dropped entirely for the same reason,
// one step earlier.
func auditablePath(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return "/" + trimmed[:i] + "/…"
	}
	return "/" + trimmed
}

func strOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
