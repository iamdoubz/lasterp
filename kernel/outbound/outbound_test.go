// SPDX-License-Identifier: AGPL-3.0-only

//go:build integrity

package outbound

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// WP-3.2a's guard, WP-3.3c's package. Invariants: **INV-X1** (the address a
// caller actually reaches is bounded, not the name it asked for), INV-T4 and
// INV-K1 in the shape the audit row takes (proven end to end where a database
// exists — kernel/plugins/http_test.go and internal/app/webhook_integrity_test.go).
//
// These are the pure halves: no database, because a guard that needs one to be
// checked is a guard nobody checks.

// TestNonPublicAddressesAreRefused moved here from kernel/plugins with the
// guard itself (WP-3.3c). Same table, same reason: the check runs on the
// resolved address, so a name that resolves into the private network is refused
// at the socket rather than trusted in a manifest.
func TestNonPublicAddressesAreRefused(t *testing.T) {
	guard := DialGuard(false)
	for _, addr := range []string{
		"127.0.0.1:443",          // loopback
		"10.1.2.3:443",           // RFC1918
		"192.168.0.5:443",        // RFC1918
		"169.254.169.254:443",    // the cloud metadata service
		"100.64.7.7:443",         // carrier-grade NAT
		"192.0.0.170:443",        // IETF protocol assignments
		"198.18.0.1:443",         // benchmarking
		"[::1]:443",              // IPv6 loopback
		"[fd00::1]:443",          // IPv6 unique-local
		"[fe80::1]:443",          // IPv6 link-local
		"[::ffff:127.0.0.1]:443", // IPv4-mapped loopback
		"[64:ff9b::7f00:1]:443",  // NAT64-embedded loopback
		"[64:ff9b::a01:203]:443", // NAT64-embedded RFC1918
	} {
		if err := guard("tcp", addr, nil); err == nil {
			t.Errorf("%s was allowed", addr)
		}
	}
	// Non-vacuity: a public address is not refused, or the guard is simply
	// "no".
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443"} {
		if err := guard("tcp", addr, nil); err != nil {
			t.Errorf("%s was refused: %v", addr, err)
		}
	}
	// And the operator's dial really does open it, or the guard above proves
	// nothing about the knob docs/09 documents.
	if err := DialGuard(true)("tcp", "127.0.0.1:443", nil); err != nil {
		t.Errorf("AllowPrivateNetworks still refused loopback: %v", err)
	}
}

func TestTargetBoundsTheDestination(t *testing.T) {
	for _, bad := range []string{
		"http://api.acme.com/x",        // plaintext
		"ftp://api.acme.com/x",         // not even http
		"file:///etc/passwd",           // no host
		"https://user:pw@api.acme.com", // credentials in the URL
		"https:///x",                   // no host
		"://nonsense",                  // not a URL
	} {
		if _, _, err := Target(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	// The port is made explicit so an allowlist compares like with like: a
	// manifest saying `api.acme.com` means 443 and nothing else.
	for target, want := range map[string]string{
		"https://api.acme.com/x":      "api.acme.com:443",
		"https://API.Acme.COM/x":      "api.acme.com:443",
		"https://api.acme.com:8443/x": "api.acme.com:8443",
	} {
		_, hostPort, err := Target(target)
		if err != nil {
			t.Fatalf("Target(%q): %v", target, err)
		}
		if hostPort != want {
			t.Errorf("Target(%q) host = %q, want %q", target, hostPort, want)
		}
	}
}

// TestAuditablePathKeepsOnlyTheRouteFamily is INV-K1 on the one field of the
// audit row that could carry a credential: for a whole class of webhook APIs
// the path *is* the secret.
func TestAuditablePathKeepsOnlyTheRouteFamily(t *testing.T) {
	for path, want := range map[string]string{
		"":                                "/",
		"/":                               "/",
		"/send":                           "/send",
		"/services/T000/B000/xoxb-secret": "/services/…",
		"/v1/send":                        "/v1/…",
	} {
		if got := AuditablePath(path); got != want {
			t.Errorf("AuditablePath(%q) = %q, want %q", path, got, want)
		}
	}
	if strings.Contains(AuditablePath("/services/T000/B000/xoxb-secret"), "xoxb-secret") {
		t.Fatal("the credential survived truncation")
	}
}

// TestDialerIsBuiltOnlyHere is the structural half, in the style of
// TestCELIsImportedOnlyHere (WP-3.3a) and TestEveryGrantSetIsChecked.
//
// The SSRF guard lives in a `net.Dialer`'s Control hook, so "there is one
// Dialer" is "there is one guard". A second one is a second outbound surface
// that will drift from this one, and the drift is found by whoever finds
// 169.254.169.254 in an audit log.
//
// It does *not* claim this is the only `http.Client` in the tree — it is not,
// and should not be: the CLI, the OIDC client and the ECB rate feed each talk
// to an operator-configured destination, none of them on behalf of untrusted
// code. What must not exist twice is the dialer that decides whether an
// address chosen by a plugin or an automation may be reached.
func TestDialerIsBuiltOnlyHere(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	here := filepath.Join(root, "kernel", "outbound")

	var offenders []string
	found := false
	inspect := func(path string) func(ast.Node) bool {
		return func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Dialer" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "net" {
				return true
			}
			if strings.HasPrefix(path, here) {
				found = true
				return true
			}
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
			return true
		}
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "bin", "dist":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// A file that does not parse is skipped, not fatal: a build failure is
		// louder than anything this gate could say about it.
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr == nil {
			ast.Inspect(file, inspect(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Non-vacuity: the scan must actually find this package's dialer, or a
	// broken walk reports a clean tree.
	if !found {
		t.Fatal("the scan did not find kernel/outbound's own net.Dialer — it is not looking where it thinks it is")
	}
	if len(offenders) > 0 {
		t.Fatalf("net.Dialer is built outside kernel/outbound (a second SSRF guard, or none): %s",
			strings.Join(offenders, ", "))
	}
}
