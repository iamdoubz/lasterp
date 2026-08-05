// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"context"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// ErrHookRejected marks a write refused by a plugin hook. It lives here, not in
// kernel/plugins, because the API gateway has to classify it (422 — a
// well-formed request refused by a business rule) and the gateway knows about
// metadata, not about plugins. The plugin's own error is wrapped inside, so a
// caller that cares which plugin refused can still find out.
var ErrHookRejected = errors.New("metadata: write rejected by a plugin hook")

// Verb names the write a hook fires on.
const (
	VerbCreate = "create"
	VerbUpdate = "update"
	VerbDelete = "delete"
)

// Hooks is the synchronous plugin dispatch seam (WP-3.1b, ADR-007's
// `before_validate`/`before_commit`).
//
// It is declared **here**, in the package that consumes it, and implemented in
// kernel/plugins — which imports this package for CRUD and therefore cannot be
// imported back (CLAUDE.md: no import cycles; small interfaces at the consumer
// side, as kernel/secrets' Grants already does).
//
// It lives inside CRUD rather than wrapping it at the composition root because
// a hook surface with a bypass is not one: wrapping would cover the HTTP routes
// and the offline drain — both of which go through the ordinary routes — and
// miss a module calling CRUD.Create directly.
//
// **Before runs before the transaction opens, never inside it.** That is what
// keeps a plugin from holding a Postgres transaction, or SQLite's single write
// lock, open for the length of its wall-clock budget — and it is what makes
// INV-X2 structural: no plugin code runs inside a transaction, so none can
// partially commit one (WP-3.1-decisions.md §8).
//
// There is deliberately no After: a committed change is already published to
// the change feed, and the async runner delivers `after_*` from there. An
// in-request after-hook would add plugin latency to a write whose outcome it
// cannot change.
type Hooks interface {
	// Before runs every sync hook registered for (object, verb) and returns the
	// record to write. A hook may enrich the record — defaulting a field,
	// normalising a code — which is half of what before_validate is for; the
	// returned record is re-validated by the caller, so enrichment can never
	// introduce a value the schema forbids (INV-T5).
	//
	// A non-nil error rejects the write. The caller surfaces it unchanged, so
	// the error must already be safe to show a user and must name the plugin
	// that refused.
	Before(ctx context.Context, tenant tenancy.ID, object, verb string, rec Record) (Record, error)
}

// WithHooks returns a copy of c that dispatches to h. A nil CRUD.hooks is
// today's behaviour exactly, which is what every test and every deployment
// without plugins gets.
func (c *CRUD) WithHooks(h Hooks) *CRUD {
	clone := *c
	clone.hooks = h
	return &clone
}

// runBefore dispatches and re-validates. It is a no-op when no hooks are wired.
//
// partial mirrors the caller's own validation mode: an update validates only
// the keys present, so a hook enriching an update is held to the same rule.
func (c *CRUD) runBefore(ctx context.Context, tenant tenancy.ID, verb string, rec Record, partial bool) (Record, error) {
	if c.hooks == nil {
		return rec, nil
	}
	out, err := c.hooks.Before(ctx, tenant, c.schema.ObjectName, verb, rec)
	if err != nil {
		// Both sentinels stay matchable: the gateway keys on this one, callers
		// inside the server can still reach the plugin package's.
		return nil, fmt.Errorf("%w: %w", ErrHookRejected, err)
	}
	if out == nil {
		return rec, nil
	}
	// Re-validated, always: a plugin is untrusted, and "the hook already saw a
	// valid record" says nothing about what it handed back (INV-T5).
	return c.validated(out, partial)
}
