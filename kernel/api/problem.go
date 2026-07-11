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
