// SPDX-License-Identifier: AGPL-3.0-only

// Package outbound is the one way anything inside this server reaches a host
// outside it (WP-3.2a, generalised by WP-3.3c; ADR-007, docs/05).
//
// It was `kernel/plugins/http.go` and it moved here when an automation needed
// the same thing. The plugin-shaped half stayed behind — the manifest
// allowlist, the plugin principal — and what is here is what a *caller* is:
// an https-only target parse, a per-call allowlist decision the caller
// supplies, an audit row written before the socket opens, and a dialer that
// refuses a non-public address.
//
// **The point of one package is that there is one dialer guard.** A second copy
// of `dialGuard` is a second SSRF surface, it will drift from the first, and
// the drift is discovered by whoever finds 169.254.169.254 in an audit log.
// `TestOutboundClientIsBuiltOnlyHere` asserts nothing else builds one.
package outbound

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
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

	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Policy is the deployment's outbound posture. It is set by the operator for
// the whole host, never by a plugin or an automation: neither can ask to reach
// the internal network, an operator can decide that they may.
type Policy struct {
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

// Outbound bounds. Small on purpose: an extension point is not a proxy, and a
// 1MB reply is already more JSON than a hook should be reading inside a write
// path.
const (
	MaxRequestBytes    = 256 << 10
	MaxResponseBytes   = 1 << 20
	maxResponseHeaders = 64
	dialTimeout        = 5 * time.Second
	callTimeout        = 30 * time.Second
)

// ErrBlocked is every outbound refusal: a destination the caller did not
// declare, a scheme that is not https, a redirect, an address that is not
// public. One error because the caller is told "no", not which rule said so —
// the classify() rule from WP-3.1a applied to the network.
var ErrBlocked = errors.New("outbound: request blocked")

// Caller is who is making the call, as the audit row will name it.
//
// Every field is required: an unattributable outbound call is INV-T4's failure
// mode, and "the server called out" is not an answer to "who did this".
type Caller struct {
	// Object and ID are the audit row's subject — ("plugin", "com.acme.x") or
	// ("automation", "notify-ops").
	Object string
	ID     string
	// Actor is the principal, `plugin:<id>` or `automation:<id>`.
	Actor string
}

func (c Caller) valid() bool { return c.Object != "" && c.ID != "" && c.Actor != "" }

// Request is one outbound call as the caller asks for it.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
}

// Response is what came back, bounded by MaxResponseBytes.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Allow reports whether this caller may reach hostPort with method. It is the
// generalised allowlist: a plugin's is its manifest, an automation's is the
// destination row an administrator registered.
type Allow func(method, hostPort string) bool

// Do makes one guarded, audited outbound call.
//
// The order is the load-bearing part and it is WP-3.2a's verbatim: parse and
// bound the target, ask the caller's allowlist, write the audit row, *then*
// open the socket. Nothing leaves this server without a row already committed
// saying it was about to.
func Do(ctx context.Context, db *storage.DB, tenant tenancy.ID, p Policy, c Caller, allow Allow, req Request) (*Response, error) {
	if db == nil || tenant == "" {
		return nil, errors.New("outbound: a database and a tenant are required")
	}
	if !c.valid() {
		return nil, errors.New("outbound: a named caller is required for every call (INV-T4)")
	}
	if allow == nil {
		return nil, fmt.Errorf("%w: no allowlist was supplied", ErrBlocked)
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = "GET"
	}

	u, hostPort, err := Target(req.URL)
	if err != nil {
		return nil, err
	}
	// The allowlist is the ceiling, checked before anything is dialled or
	// logged. Somebody approved "this caller talks to api.acme.com with GET and
	// POST"; nothing else leaves.
	if !allow(method, hostPort) {
		return nil, fmt.Errorf("%w: %s %s is not in this caller's allowlist", ErrBlocked, method, hostPort)
	}
	if len(req.Body) > MaxRequestBytes {
		return nil, fmt.Errorf("%w: request body is %d bytes, over the %d-byte limit", ErrBlocked, len(req.Body), MaxRequestBytes)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBlocked, err)
	}
	setHeaders(httpReq, req.Headers)

	// **Audited before it is dialled.** ADR-007 requires every outbound call to
	// be audited; recording the intent before the socket opens is the only
	// ordering in which no call can happen without a row. `audit_log` is
	// append-only (docs/19 §2), so this row cannot be amended with the status
	// afterwards — the caller gets the status, the operator gets the fact of
	// the call. The URL's query string is deliberately not recorded: API keys
	// live there.
	//
	// ponytail: one row per call, request-side only. A second completion row
	// with the status is the upgrade if operators need outcomes, and it doubles
	// the volume of the noisiest audit action.
	//
	// The path is recorded to its first segment only — see AuditablePath.
	if err := audit(ctx, db, tenant, c, method, hostPort, AuditablePath(u.Path)); err != nil {
		return nil, err
	}

	resp, err := client(p).Do(httpReq)
	if err != nil {
		// Wrapped, not returned raw: a dial error carries the resolved address
		// and the caller does not need to learn the shape of the network it was
		// refused from.
		return nil, fmt.Errorf("%w: %s", ErrBlocked, ClassifyDialError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the response failed", ErrBlocked)
	}
	if len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("%w: response is over the %d-byte limit", ErrBlocked, MaxResponseBytes)
	}
	return &Response{Status: resp.StatusCode, Headers: responseHeaders(resp.Header), Body: body}, nil
}

// Target parses and bounds a destination, returning the URL and its host with
// the port made explicit so an allowlist compares like with like.
func Target(target string) (*url.URL, string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %q is not a URL", ErrBlocked, target)
	}
	// https only. Plaintext outbound from a server holding tenant data is not
	// a knob, and it is the difference between "this caller talks to
	// api.acme.com" and "this caller talks to whoever is on the path".
	if u.Scheme != "https" {
		return nil, "", fmt.Errorf("%w: scheme %q is not https", ErrBlocked, u.Scheme)
	}
	if u.User != nil {
		return nil, "", fmt.Errorf("%w: credentials in the URL are not accepted", ErrBlocked)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, "", fmt.Errorf("%w: no host", ErrBlocked)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return u, host + ":" + port, nil
}

// setHeaders copies the caller's headers onto the request. Hop-by-hop and
// identity-bearing headers are dropped: Host would make the allowlisted
// destination a lie, and the rest are the transport's business.
func setHeaders(r *http.Request, headers map[string]string) {
	for k, value := range headers {
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

// responseHeaders renders the reply's headers for the caller, capped.
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

// audit writes the INV-T4 row: who called out, where, and when. No headers and
// no bodies — a caller that reads `acme_api_key` from the vault and sends it as
// a bearer token would otherwise write that secret into the audit log in the
// clear, which is exactly what INV-K1 forbids. This row says a call happened;
// it is not a proxy log.
func audit(ctx context.Context, db *storage.DB, tenant tenancy.ID, c Caller, method, hostPort, path string) error {
	changes, err := json.Marshal(map[string]any{
		c.Object: c.ID, "method": method, "host": hostPort, "path": path,
	})
	if err != nil {
		return err
	}
	err = tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, db.Rebind(`
			INSERT INTO audit_log (id, tenant_id, object, record_id, action, changes, actor_id, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			idgen.New(), string(tenant), c.Object, c.ID, "http.request", string(changes),
			c.Actor, time.Now().UTC())
		return err
	})
	if err != nil {
		// Unaudited calls do not happen: if the row cannot be written, the
		// request is not made. ADR-007 says audited, and "we tried" is not
		// audited.
		return fmt.Errorf("%w: the call could not be audited: %w", ErrBlocked, err)
	}
	return nil
}

// client returns the shared client for a policy. One exists per posture for the
// life of the process, so connections pool and the guard below is installed
// exactly once — a deployment has one policy, so in production this map holds
// one entry.
var (
	clientsMu sync.Mutex
	clients   = map[Policy]*http.Client{}
)

func client(p Policy) *http.Client {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clients[p]; ok {
		return c
	}
	c := newClient(p)
	clients[p] = c
	return c
}

func newClient(p Policy) *http.Client {
	dialer := &net.Dialer{Timeout: dialTimeout, Control: DialGuard(p.AllowPrivateNetworks)}
	return &http.Client{
		Timeout: callTimeout,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
			// nil RootCAs is the system trust store, which is what https-only
			// is for; a deployment with an internal CA supplies its own.
			TLSClientConfig: &tls.Config{RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12},
		},
		// **Redirects are not followed.** A 302 to a host the allowlist never
		// named is how an allowlist is bypassed in one hop, and following it
		// would re-run the guard on an address the administrator never
		// approved. The 3xx is handed back to the caller, which may re-request
		// inside its own allowlist.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// DialGuard refuses to open a socket to a non-public address.
//
// It runs in Dialer.Control, which fires **after** resolution with the address
// the kernel is about to connect to, so an allowlisted name that resolves to
// 169.254.169.254 — the cloud metadata service, the classic SSRF prize — is
// refused on the address rather than trusted on the name. That placement is
// also what makes DNS rebinding pointless: there is no window between the check
// and the connection for a second answer to land in.
//
// Exported so the one guard can be tested directly. It is still the *only* one:
// nothing outside this package builds a client.
func DialGuard(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrBlocked, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !isPublicIP(ip) {
			return fmt.Errorf("%w: %s is not a public address", ErrBlocked, host)
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

// ClassifyDialError keeps server network topology out of a caller's error
// string while still telling it apart from a refusal it can fix.
func ClassifyDialError(err error) string {
	if errors.Is(err, ErrBlocked) {
		return "the destination address is not permitted"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the request did not finish inside this call's budget"
	}
	return "the request failed"
}

// AuditablePath is the part of a URL path safe to write down: the first
// segment, and nothing after it.
//
// The full path cannot be recorded, because for a whole class of APIs **the
// path is the credential** — a Slack incoming webhook is
// `/services/T000/B000/<secret>`, and so is every "unguessable URL" integration
// built the same way. Writing that into `audit_log` would put a live credential
// in the trail of the call that used it, which is precisely INV-K1, and the
// caller holding it did nothing wrong: it was granted the secret and the
// destination and used both as intended.
//
// The first segment is kept because it is what makes a row *useful* to an
// operator — `/services` and `/admin` on the same host are different answers to
// "what is this thing doing" — and because a first segment is a route family,
// not an identifier. The query string is dropped entirely for the same reason,
// one step earlier.
func AuditablePath(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return "/" + trimmed[:i] + "/…"
	}
	return "/" + trimmed
}
