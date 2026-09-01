// SPDX-License-Identifier: AGPL-3.0-only

package app

import (
	"net/http"
	"sort"

	"github.com/iamdoubz/lasterp/kernel/api"
	"github.com/iamdoubz/lasterp/kernel/capability"
	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// metaField is one field as the client renderer consumes it. It is a narrowed
// projection of metadata.Field, not the struct itself: the renderer needs the
// shape of the data, and nothing else in the schema is its business.
type metaField struct {
	Name     string             `json:"name"`
	Type     metadata.FieldType `json:"type"`
	Required bool               `json:"required"`
	Target   string             `json:"target,omitempty"`
	// Localized tells the renderer this field's value may exist in several
	// languages, so it should show (and edit) the one for the current locale
	// out of the record's translations rather than only the canonical value.
	Localized bool `json:"localized,omitempty"`

	// Options is an enum field's closed value set — the same list the engine
	// validates writes against, so the renderer offers exactly what the server
	// will accept rather than a free-text box and a 422.
	Options []string `json:"options,omitempty"`

	// Order, Group and Widget are the UI descriptors WP-1.5 §2 deferred.
	// Order is presentation only; it never reflects storage order.
	Order  int    `json:"order,omitempty"`
	Group  string `json:"group,omitempty"`
	Widget string `json:"widget,omitempty"`
}

// metaObject is one renderable object: its name, its resource path, and its
// fields in presentation order (WP-1.11 gave the schema real UI descriptors;
// before that, field order was declaration order and nothing could change it).
type metaObject struct {
	Name     string `json:"name"`
	Resource string `json:"resource"`
	Module   string `json:"module"`

	// Persistence tells a replica which objects it can hydrate. Event-sourced
	// objects have no CRUD surface, so /api/v1/sync/snapshot 404s for them
	// (sync.go newResolver) — and without this field a client generating its
	// schema from this endpoint has no way to know that before asking.
	//
	// The alternatives were to treat that 404 as "not replicable", which
	// conflates it with an unknown object and a disabled module, or to keep the
	// event-sourced list client-side, which is the hand-written duplicate of
	// metadata that ADR-006 and CLAUDE.md both forbid. See
	// WP-2.2b-decisions.md §2.
	Persistence metadata.Persistence `json:"persistence"`

	Fields []metaField `json:"fields"`
}

// metaActions serves the effective object schemas the web client renders
// list/form/detail from. Authenticated and tenant-scoped: overlays are
// per-tenant (ADR-006), so this is an INV-T1 read path like any other, not a
// static document. OpenAPI is not a substitute — it describes routes, not the
// field-level metadata a renderer needs.
func metaActions(db *storage.DB, objects []*metadata.EffectiveSchema, reg *capability.Registry) []api.Action {
	return []api.Action{
		{Method: "GET", Path: "/api/v1/meta/objects", Object: "",
			Summary: "List renderable object schemas for the current tenant",
			Handler: listMetaObjects(db, objects, reg)},
	}
}

// listMetaObjects returns the schemas for objects whose module is enabled for
// the calling tenant. Filtering here rather than letting the client discover
// 403s per object keeps the navigation honest: a disabled module simply has no
// screens.
//
// Each surviving object is resolved against the tenant's overlays (WP-3.2c), so
// a custom field reaches the renderer — and `lasterp plugin bindings`, which
// generates typed accessors from this endpoint — without either of them knowing
// overlays exist.
func listMetaObjects(db *storage.DB, objects []*metadata.EffectiveSchema, reg *capability.Registry) api.HandlerFunc {
	checker := capability.GatewayChecker{Reg: reg, DB: db}
	resolver := metadata.DBResolver{DB: db}
	return func(w http.ResponseWriter, r *http.Request, tenant tenancy.ID) {
		out := make([]metaObject, 0, len(objects))
		for _, s := range objects {
			enabled, _, err := checker.Enabled(r.Context(), tenant, s.ObjectName)
			if err != nil {
				fail(w, r, err)
				return
			}
			if !enabled {
				continue
			}
			core := s.Object
			eff, err := resolver.Resolve(r.Context(), tenant, &core)
			if err != nil {
				fail(w, r, err)
				return
			}
			out = append(out, toMetaObject(eff))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

// toMetaObject projects an effective schema for the renderer. Fields come out
// in presentation order as well as carrying their `order`, so a client that
// does not sort is still right — two consumers disagreeing about field order
// would be a confusing way to learn this attribute exists.
func toMetaObject(s *metadata.EffectiveSchema) metaObject {
	ordered := s.PresentationOrder()
	fields := make([]metaField, 0, len(ordered))
	for _, f := range ordered {
		fields = append(fields, metaField{
			Name: f.Name, Type: f.Type, Required: f.Required, Target: f.Target,
			Localized: f.Localized, Options: f.Options,
			Order: f.Order, Group: f.Group, Widget: string(f.Widget),
		})
	}
	return metaObject{
		Name:        s.ObjectName,
		Resource:    api.ResourcePath(s.ObjectName),
		Module:      s.Module,
		Persistence: s.Persistence,
		Fields:      fields,
	}
}
