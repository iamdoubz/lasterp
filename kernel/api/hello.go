// SPDX-License-Identifier: AGPL-3.0-only

// Package api hosts the kernel HTTP surface. WP-0.1 wires only a
// health check and a hello-world route to prove the bootstrap.
package api

import (
	"encoding/json"
	"net/http"
)

// NewMux returns the kernel API's HTTP handler.
func NewMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /api/v1/hello", handleHello)
	return mux
}

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
