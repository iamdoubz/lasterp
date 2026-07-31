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
}

// metaObject is one renderable object: its name, its resource path, and its
// fields in schema order (WP-1.5-decisions.md §2 — field order is schema order
// until the object schema grows real UI descriptors).
type metaObject struct {
	Name     string      `json:"name"`
	Resource string      `json:"resource"`
	Module   string      `json:"module"`
	Fields   []metaField `json:"fields"`
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
func listMetaObjects(db *storage.DB, objects []*metadata.EffectiveSchema, reg *capability.Registry) api.HandlerFunc {
	checker := capability.GatewayChecker{Reg: reg, DB: db}
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
			out = append(out, toMetaObject(s))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, http.StatusOK, map[string]any{"data": out})
	}
}

func toMetaObject(s *metadata.EffectiveSchema) metaObject {
	fields := make([]metaField, 0, len(s.Fields))
	for _, f := range s.Fields {
		fields = append(fields, metaField{
			Name: f.Name, Type: f.Type, Required: f.Required, Target: f.Target,
		})
	}
	return metaObject{
		Name:     s.ObjectName,
		Resource: api.ResourcePath(s.ObjectName),
		Module:   s.Module,
		Fields:   fields,
	}
}
