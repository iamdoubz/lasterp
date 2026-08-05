// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/modules/invoicing"
	"github.com/iamdoubz/lasterp/modules/ledger"
	"github.com/iamdoubz/lasterp/modules/reporting"
)

// writeJSON renders body as application/json with status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeProblem renders an RFC 7807 problem+json document (matching
// kernel/api's problem shape) for the action routes.
func writeProblem(w http.ResponseWriter, status int, title, detail, instance string) {
	writeProblemTyped(w, "about:blank", status, title, detail, instance)
}

// writeProblemTyped is writeProblem with a machine-readable `type`, for the few
// failures a client must branch on rather than merely display.
func writeProblemTyped(w http.ResponseWriter, problemType string, status int, title, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": problemType, "title": title, "status": status,
		"detail": detail, "instance": instance,
	})
}

// fail maps a domain error to a problem+json response. Known sentinels
// classify explicitly; anything unrecognized is a 500 with no leaked detail
// (the concrete error stays server-side), matching api.problemForError.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	inst := r.URL.Path
	switch {
	case errors.Is(err, context.Canceled):
		// The caller hung up mid-request (navigated away, closed the tab).
		// Nothing was lost and nobody is reading the response; logging it as an
		// unhandled failure buries real 500s in noise.
		writeProblem(w, http.StatusInternalServerError, "request cancelled", "", inst)
	case errors.Is(err, authz.ErrPermissionDenied):
		writeProblem(w, http.StatusForbidden, "permission denied", "", inst)
	case errors.Is(err, authz.ErrNoActor):
		writeProblem(w, http.StatusUnauthorized, "authentication required", "", inst)
	case errors.Is(err, reporting.ErrUnknownReport), errors.Is(err, reporting.ErrUnknownMetric),
		errors.Is(err, reporting.ErrUnknownDashboard):
		writeProblem(w, http.StatusNotFound, "unknown report, metric or dashboard", err.Error(), inst)
	case errors.Is(err, capability.ErrUnknownModule):
		writeProblem(w, http.StatusNotFound, "unknown module", err.Error(), inst)
	case isNotFound(err):
		writeProblem(w, http.StatusNotFound, "not found", err.Error(), inst)
	case isConflict(err):
		writeProblem(w, http.StatusConflict, "conflict", err.Error(), inst)
	case isUnprocessable(err):
		writeProblem(w, http.StatusUnprocessableEntity, "unprocessable", err.Error(), inst)
	default:
		// The detail stays out of the response (it can carry internals), but it
		// must not vanish: an unclassified 500 with no log anywhere is
		// undebuggable in production and in CI alike.
		//
		// The path is %q, not %s: it is attacker-controlled, and an unquoted
		// newline in it forges a log line. This product's pitch includes
		// tamper-evident trails, so the ordinary log should not be the one place
		// anyone can write whatever they like.
		// #nosec G706 -- %q is the sanitizer (control characters escaped);
		// gosec only sees request data reaching a log call and cannot tell
		// which verb formatted it.
		log.Printf("unhandled error on %s %q: %v", r.Method, inst, err)
		writeProblem(w, http.StatusInternalServerError, "internal server error", "", inst)
	}
}

func isNotFound(err error) bool {
	return errors.Is(err, metadata.ErrRecordNotFound) ||
		errors.Is(err, invoicing.ErrInvoiceNotFound) ||
		errors.Is(err, invoicing.ErrReceiptNotFound) ||
		errors.Is(err, ledger.ErrEntryNotFound) ||
		errors.Is(err, ledger.ErrPeriodNotFound)
}

// A referenced account that does not exist is a bad request body, not a server
// fault — it surfaced as an opaque 500 until the WP-1.5 e2e hit it.

// isConflict covers wrong-state transitions (the resource exists but the
// requested change is not allowed from its current state).
func isConflict(err error) bool {
	// Disabling a module something else depends on is a legitimate request
	// refused for a legitimate reason (ADR-018 dependency closure) — the caller
	// needs to see which modules block it, not an opaque 500.
	var inUse capability.ErrModuleInUse
	return errors.Is(err, metadata.ErrIDTaken) ||
		errors.Is(err, invoicing.ErrNotDraft) ||
		errors.Is(err, ledger.ErrPeriodNotOpen) ||
		errors.Is(err, ledger.ErrPeriodNotClosed) ||
		errors.Is(err, capability.ErrKernelNotDisableable) ||
		errors.As(err, &inUse)
}

// isUnprocessable covers well-formed requests that violate a business rule
// (validation, balance, closed period, missing reference data).
func isUnprocessable(err error) bool {
	return errors.Is(err, metadata.ErrValidation) ||
		// A plugin hook vetoed the write (WP-3.1b): a well-formed request
		// refused by a business rule, which is exactly what 422 means. The
		// detail names the plugin, because a user told only "rejected" cannot
		// act and an administrator cannot find the cause.
		errors.Is(err, metadata.ErrHookRejected) ||
		// A dashboard asked for a period the tenant does not have, or has no
		// fiscal calendar at all: a well-formed request the books cannot answer.
		errors.Is(err, reporting.ErrNoPeriods) ||
		errors.Is(err, reporting.ErrUnknownPeriod) ||
		// Translating a field nobody declared localized, and every other
		// structural complaint about a draft, is a bad request body — not a
		// server fault.
		errors.Is(err, metadata.ErrNotLocalized) ||
		errors.Is(err, invoicing.ErrInvalidDraft) ||
		// Over-applying a receipt is a well-formed request that breaks a
		// business rule (INV-F8) — the caller needs to see which invoice and
		// by how much, not a 500.
		errors.Is(err, invoicing.ErrOverApplied) ||
		errors.Is(err, ledger.ErrAccountNotFound) ||
		errors.Is(err, ledger.ErrClosedPeriod) ||
		errors.Is(err, ledger.ErrUnbalanced) ||
		errors.Is(err, ledger.ErrTooFewLines) ||
		errors.Is(err, ledger.ErrLineNotXOR) ||
		errors.Is(err, ledger.ErrNegativeAmount)
}

// decodeJSON parses a JSON object body into dst, writing a 400 and reporting
// false on malformed input.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeProblem(w, http.StatusBadRequest, "malformed JSON body", err.Error(), r.URL.Path)
		return false
	}
	return true
}
