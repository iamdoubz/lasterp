// SPDX-License-Identifier: AGPL-3.0-only

package changefeed

// ScopeKeyFor returns the scope key an entry is tagged with — the label a
// client's sync scope is matched against when WP-2.4 computes which subset of
// the feed a device is entitled to (docs/04 §Concepts).
//
// This WP writes the tag and indexes it; it does not compute scopes. Role-based
// scope computation, re-shaping on entitlement change, and revocation purges
// are WP-2.4's acceptance criteria, and a scope engine built before there is a
// client to shape for would be designed twice.
//
// So the key is the object name, which is the coarsest useful grouping and the
// one every later refinement subdivides rather than replaces: a scope over
// "Invoice" stays expressible when scopes become "my region's invoices". What
// this WP owes WP-2.4 is that the column, its index and this call site exist,
// so that WP replaces a function body instead of migrating a populated table.
//
// ponytail: object-name granularity. Narrower keys (region, owner, document
// age) land with WP-2.4's scope computation.
func ScopeKeyFor(object string) string {
	if object == "" {
		return "unscoped"
	}
	return object
}
