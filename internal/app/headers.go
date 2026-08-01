// SPDX-License-Identifier: AGPL-3.0-only

package app

import "net/http"

// Security headers for every response, API and bundle alike (WP-1.10).
//
// docs/08 has listed CSP and HSTS under "standard web hardening" since Phase 0
// and nothing set a single header until now. That mattered more here than the
// generic case: the SPA renders tenant-authored strings through the metadata
// renderer, and PDF/CSV/XLSX downloads are served from the same origin as the
// session cookie, so a browser willing to guess a content type is a browser that
// can be talked into executing a document.
//
// These live in internal/app rather than kernel/api because the policy depends
// on what is being served — the built bundle — and kernel/api is the headless
// metadata API surface, which knows nothing about a web client. The composition
// root is where the two are joined, so it is where the policy covering both
// belongs (WP-1.10-decisions.md §4).
const (
	// contentSecurityPolicy is deliberately strict and deliberately not
	// configurable. The built bundle has no inline script and no inline style —
	// Vite emits a hashed module script and a stylesheet link, and Tailwind v4
	// compiles to a file — so 'self' is genuinely sufficient and there is no
	// pressure to add 'unsafe-inline'. A configurable policy whose default
	// nobody tests decays into 'unsafe-inline' the first time someone hits a
	// wall (decisions §2).
	//
	// data: is allowed for images only, because the metadata renderer may inline
	// small ones. frame-ancestors 'none' is the modern form of the
	// X-Frame-Options below; both are sent, since the older header is what old
	// browsers understand.
	contentSecurityPolicy = "default-src 'self'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"font-src 'self'; " +
		"connect-src 'self'; " +
		"object-src 'none'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'"

	// strictTransportSecurity is sent unconditionally. Browsers ignore HSTS on a
	// plaintext response by design (RFC 6797 §8.1), so always sending it is both
	// safe and zero-config; the alternative — sniffing X-Forwarded-Proto — means
	// trusting a header from the edge, which this product already refuses to do
	// for client IPs (the rate limiter keys off RemoteAddr, never
	// X-Forwarded-For). Applying a weaker trust rule to the same class of header
	// for a lesser reason would be incoherent.
	//
	// No includeSubDomains: LastERP does not know what else lives on its parent
	// domain, and a self-hoster at erp.example.com should not have an unrelated
	// wiki.example.com forced to HTTPS by our default. No preload: that is an
	// irreversible submission to a browser-vendor list and is the operator's
	// call, not an application server's (decisions §3).
	strictTransportSecurity = "max-age=31536000"

	// referrerPolicy is no-referrer rather than the more common
	// strict-origin-when-cross-origin: ERP URLs carry tenant ids, invoice ids and
	// report parameters, and none of that should reach a third party because a
	// user followed a link out.
	referrerPolicy = "no-referrer"
)

// withSecurityHeaders sets the headers on every response before handing off.
// They are set rather than added, so a handler that has already made a decision
// about one of them keeps it — nothing does today, and if something ever needs
// to (a deliberately embeddable page, say), it can override after this runs.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		// The one header that actually stops "a PDF full of tenant-authored
		// text" from being sniffed as HTML and executed on our origin.
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Strict-Transport-Security", strictTransportSecurity)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", referrerPolicy)
		next.ServeHTTP(w, r)
	})
}
