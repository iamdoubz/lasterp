// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// Problem is an RFC 7807 problem+json document (ADR-009: "consistent
// RFC-7807 problem responses"). type is a URI reference identifying the
// problem kind; we use "about:blank" plus an HTTP status when there is no
// more specific type, per the RFC.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// ProblemIdempotencyConflict is the `type` on the 409 the idempotency store
// raises. It is exported because a client has to branch on it: the same
// status covers a key reused with a different request and a key whose
// original request has not finished yet, and only the second is worth
// retrying. See gateway.go and WP-2.3-decisions.md §11.
const ProblemIdempotencyConflict = "idempotency-conflict"

// ProblemDeviceWiped is the `type` on the 401 a remotely-wiped device gets
// (WP-2.5, INV-D1). Exported for the same reason as the constant above: the
// client must branch on it, and here the branch is the entire feature.
//
// An undifferentiated 401 makes a client sign the user out and leave the
// replica on disk. Only this type tells it to destroy what it holds — which is
// why identity.ErrDeviceWiped is the one authentication outcome allowed to be
// distinguishable from the deliberately-opaque ErrSessionInvalid
// (WP-2.5-decisions.md §3).
const ProblemDeviceWiped = "device-wiped"

// writeProblem renders p as application/problem+json.
func writeProblem(w http.ResponseWriter, p Problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// problemForError maps a domain error to a problem+json response, by
// errors.Is so wrapped errors still classify. Unknown errors become a 500
// with no internal detail leaked (the concrete error text stays server-side).
func problemForError(err error, instance string) Problem {
	switch {
	case errors.Is(err, metadata.ErrValidation):
		return Problem{Status: http.StatusUnprocessableEntity, Title: "validation failed", Detail: err.Error(), Instance: instance}
	case errors.Is(err, metadata.ErrNotLocalized):
		// Supplying translations for a field the schema does not localize is a
		// bad request body: nothing would ever read them back.
		return Problem{Status: http.StatusUnprocessableEntity, Title: "field is not localized", Detail: err.Error(), Instance: instance}
	case errors.Is(err, metadata.ErrIDTaken):
		// Detail deliberately says nothing beyond "taken". The primary key is
		// global per table, so naming the holder — or even confirming it is a
		// live row of this tenant — would turn a caller-supplied id into an
		// existence oracle across tenants (INV-T1).
		return Problem{Status: http.StatusConflict, Title: "id already exists", Detail: "the supplied id is already taken", Instance: instance}
	case errors.Is(err, metadata.ErrRecordNotFound):
		return Problem{Status: http.StatusNotFound, Title: "record not found", Instance: instance}
	case errors.Is(err, authz.ErrPermissionDenied):
		return Problem{Status: http.StatusForbidden, Title: "permission denied", Instance: instance}
	case errors.Is(err, authz.ErrNoActor):
		return Problem{Status: http.StatusUnauthorized, Title: "authentication required", Instance: instance}
	default:
		return Problem{Status: http.StatusInternalServerError, Title: "internal server error", Instance: instance}
	}
}
