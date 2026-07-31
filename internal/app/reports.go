// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"strings"
	"time"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
	"github.com/iamdoubz/lasterp/modules/reporting"
)

// Reporting is read-only, so every route here is a GET with no idempotency key
// and no capability object of its own — the permission that matters is the one
// each report and metric declares, checked inside the engine (docs/21 §1,
// INV-T1). Gating these on a capability as well would only decide whether the
// endpoint exists, not what it may show.
func reportActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/reports", Object: "",
			Summary: "List available reports", Handler: listReports()},
		{Method: "GET", Path: "/api/v1/reports/{name}", Object: "",
			Summary: "Run a report", Handler: runReport(db)},
		{Method: "GET", Path: "/api/v1/reports/{name}/export", Object: "",
			Summary: "Run a report and export it as CSV or XLSX", Handler: exportReport(db)},
		{Method: "GET", Path: "/api/v1/metrics", Object: "",
			Summary: "List and evaluate the metrics the caller may see", Handler: listMetrics(db)},
		{Method: "GET", Path: "/api/v1/metrics/{name}", Object: "",
			Summary: "Evaluate one metric", Handler: getMetric(db)},
	}
}

// reportParams reads the query scope. Currency is required rather than defaulted
// because a report silently rendered in the wrong currency is worse than one
// that refuses: totals would look plausible and be meaningless.
func reportParams(r *http.Request) (reporting.Scope, error) {
	p := reporting.Scope{Currency: strings.ToUpper(r.URL.Query().Get("currency"))}
	if p.Currency == "" {
		return p, errMissingCurrency
	}
	if raw := r.URL.Query().Get("as_of"); raw != "" {
		at, err := time.Parse(dateLayout, raw)
		if err != nil {
			return p, err
		}
		p.AsOf = at
	} else {
		p.AsOf = time.Now().UTC()
	}
	return p, nil
}

// errMissingCurrency is its own value so the handler can answer 400 with a
// useful message rather than letting it fall through as a generic failure.
var errMissingCurrency = &paramError{"currency query parameter is required"}

type paramError struct{ msg string }

func (e *paramError) Error() string { return e.msg }

func listReports() api.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request, _ tenancy.ID) {
		writeJSON(w, http.StatusOK, map[string]any{"data": reporting.Reports()})
	}
}

func runReport(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		p, err := reportParams(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid report parameters", err.Error(), r.URL.Path)
			return
		}
		rep, err := reporting.Run(r.Context(), db, tenant, r.PathValue("name"), p)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	}
}

// exportReport renders a report as a downloadable file. Format comes from
// ?format=csv|xlsx, defaulting to CSV.
func exportReport(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		p, err := reportParams(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid report parameters", err.Error(), r.URL.Path)
			return
		}
		name := r.PathValue("name")
		// Run goes through the same permission gate as the JSON route: an export
		// must never be a way around a refusal.
		rep, err := reporting.Run(r.Context(), db, tenant, name, p)
		if err != nil {
			fail(w, r, err)
			return
		}

		format := r.URL.Query().Get("format")
		if format == "" {
			format = "csv"
		}
		var body []byte
		var contentType, ext string
		switch format {
		case "csv":
			body, err = reporting.ExportCSV(rep)
			contentType, ext = "text/csv; charset=utf-8", "csv"
		case "xlsx":
			body, err = reporting.ExportXLSX(rep)
			contentType, ext = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "xlsx"
		default:
			writeProblem(w, http.StatusBadRequest, "unsupported export format",
				"format must be csv or xlsx", r.URL.Path)
			return
		}
		if err != nil {
			fail(w, r, err)
			return
		}

		// The filename is built from the report name (a known catalog key) and
		// the date, never from user input, so there is nothing to inject into
		// the header.
		filename := name + "-" + p.AsOf.Format(dateLayout) + "." + ext
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// listMetrics evaluates every metric the caller may see. Metrics they may not
// see are omitted, not errored — a dashboard renders what the viewer is entitled
// to and stays silent about the rest.
func listMetrics(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		p, err := reportParams(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid metric parameters", err.Error(), r.URL.Path)
			return
		}
		values, err := reporting.EvaluateAll(r.Context(), db, tenant,
			p)
		if err != nil {
			fail(w, r, err)
			return
		}
		if values == nil {
			values = []reporting.Value{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": values, "catalog": reporting.Metrics()})
	}
}

func getMetric(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		p, err := reportParams(r)
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid metric parameters", err.Error(), r.URL.Path)
			return
		}
		v, err := reporting.Evaluate(r.Context(), db, tenant, r.PathValue("name"),
			p)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// --- dashboards (WP-1.8) ---

// dashboardActions serves the role packs. Like reports these are GETs with no
// capability object of their own: what a viewer may see is decided per tile by
// the metric's own permission, inside the engine (docs/21 §1, INV-T1).
func dashboardActions(db *storage.DB) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/dashboards", Object: "",
			Summary: "List role dashboards, marking which the caller can render", Handler: listDashboards(db)},
		{Method: "GET", Path: "/api/v1/dashboards/{name}", Object: "",
			Summary: "Render a role dashboard for a period", Handler: renderDashboard(db)},
	}
}

func listDashboards(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		actor, err := authz.ActorFromContext(r.Context())
		if err != nil {
			fail(w, r, err)
			return
		}
		roles, err := authz.RolesFor(r.Context(), db, actor)
		if err != nil {
			fail(w, r, err)
			return
		}
		listings, err := reporting.List(r.Context(), db, tenant, roles)
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": listings})
	}
}

func renderDashboard(db *storage.DB) api.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		currency := strings.ToUpper(r.URL.Query().Get("currency"))
		if currency == "" {
			writeProblem(w, http.StatusBadRequest, "invalid dashboard parameters",
				errMissingCurrency.Error(), r.URL.Path)
			return
		}
		// An absent period means "the current one", resolved from the tenant's
		// own fiscal calendar rather than from the server clock.
		rendered, err := reporting.Render(r.Context(), db, tenant,
			r.PathValue("name"), currency, r.URL.Query().Get("period"))
		if err != nil {
			fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, rendered)
	}
}
