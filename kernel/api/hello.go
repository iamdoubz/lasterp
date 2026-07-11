// SPDX-License-Identifier: AGPL-3.0-only

// Package api hosts the kernel HTTP surface: the metadata-driven REST
// gateway (WP-0.6) plus the health and hello bootstrap routes.
package api

import (
	"encoding/json"
	"net/http"
)

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleHello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "hello from LastERP"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
