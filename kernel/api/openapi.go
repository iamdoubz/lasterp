// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"sort"
	"strings"

	"github.com/iamdoubz/lasterp/kernel/metadata"
)

// obj is a small alias to keep the OpenAPI document literal readable.
type obj = map[string]any

// OpenAPI builds an OpenAPI 3.1 document from the registered object schemas
// and non-CRUD action routes (docs/15: "generated, not hand-written, from
// object metadata"). The result is a plain map so it marshals to JSON with
// encoding/json and needs no spec types; JSON Schema 2020-12 is the 3.1 schema
// dialect. Objects and actions are emitted in path order for a stable,
// diff-friendly spec.
func OpenAPI(schemas []*metadata.EffectiveSchema, actions []Action) obj {
	ordered := append([]*metadata.EffectiveSchema(nil), schemas...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ObjectName < ordered[j].ObjectName })

	paths := obj{
		"/healthz": obj{
			"get": obj{
				"summary":     "Liveness probe",
				"operationId": "healthz",
				"responses": obj{
					"200": obj{"description": "service is healthy"},
				},
			},
		},
		"/api/v1/openapi.json": obj{
			"get": obj{
				"summary":     "This OpenAPI document (live, tenant-aware)",
				"operationId": "openapi",
				"responses": obj{
					"200": obj{"description": "OpenAPI 3.1 document"},
				},
			},
		},
	}

	schemaComponents := obj{"Problem": problemSchema()}
	for _, s := range ordered {
		name := s.ObjectName
		ref := "#/components/schemas/" + name
		schemaComponents[name] = objectSchema(s)
		base := "/api/v1/" + resourcePath(name)
		paths[base] = collectionPath(name, ref)
		paths[base+"/{id}"] = itemPath(name, ref)
	}

	for _, a := range actions {
		p, _ := paths[a.Path].(obj)
		if p == nil {
			p = obj{}
		}
		p[strings.ToLower(a.Method)] = actionOperation(a)
		paths[a.Path] = p
	}

	return obj{
		"openapi": "3.1.0",
		"info": obj{
			"title":       "LastERP API",
			"version":     "v1",
			"description": "Metadata-generated REST API (ADR-009). Writes require an Idempotency-Key header; errors are RFC 7807 problem+json.",
		},
		"servers": []any{obj{"url": "/"}},
		"paths":   paths,
		"components": obj{
			"schemas": schemaComponents,
			"parameters": obj{
				"IdempotencyKey": idempotencyKeyParam(),
			},
			"responses": obj{
				"Problem": obj{
					"description": "RFC 7807 problem document",
					"content": obj{
						"application/problem+json": obj{
							"schema": obj{"$ref": "#/components/schemas/Problem"},
						},
					},
				},
			},
		},
	}
}

// resourcePath is the URL segment for an object: lowercased name, no
// pluralization (see WP-0.6-decisions.md).
func resourcePath(objectName string) string { return strings.ToLower(objectName) }

func objectSchema(s *metadata.EffectiveSchema) obj {
	props := obj{
		"id":          obj{"type": "string", "readOnly": true},
		"tenant_id":   obj{"type": "string", "readOnly": true},
		"created_at":  obj{"type": "string", "format": "date-time", "readOnly": true},
		"updated_at":  obj{"type": "string", "format": "date-time", "readOnly": true},
		"archived_at": obj{"type": "string", "format": "date-time", "readOnly": true},
	}
	var required []any
	for _, f := range s.Fields {
		props[f.Name] = fieldSchema(f.Type)
		if f.Required {
			required = append(required, f.Name)
		}
	}
	out := obj{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// fieldSchema maps a metadata FieldType to a JSON Schema fragment. money,
// decimal and percent are strings (exact minor-unit representation, never
// float — CLAUDE.md / kernel/metadata DDL), matching how the CRUD engine
// stores them.
func fieldSchema(t metadata.FieldType) obj {
	switch t {
	case metadata.FieldInt:
		return obj{"type": "integer"}
	case metadata.FieldBool:
		return obj{"type": "boolean"}
	case metadata.FieldDate, metadata.FieldDatetime:
		return obj{"type": "string", "format": "date-time"}
	case metadata.FieldEmail:
		return obj{"type": "string", "format": "email"}
	case metadata.FieldJSON, metadata.FieldAddress:
		return obj{"type": "object"}
	default:
		return obj{"type": "string"}
	}
}

func collectionPath(name, ref string) obj {
	lower := strings.ToLower(name)
	return obj{
		"get": obj{
			"summary":     "List " + name,
			"operationId": "list" + name,
			"responses": obj{
				"200": obj{
					"description": "a page of " + name + " records",
					"content": obj{
						"application/json": obj{
							"schema": obj{
								"type":       "object",
								"properties": obj{"data": obj{"type": "array", "items": obj{"$ref": ref}}},
							},
						},
					},
				},
				"401": problemRef(),
				"403": problemRef(),
				"429": problemRef(),
			},
		},
		"post": obj{
			"summary":     "Create a " + name,
			"operationId": "create" + name,
			"parameters":  []any{idempotencyRef()},
			"requestBody": bodyRef(ref),
			"responses": obj{
				"201": okObject(ref, "the created "+name),
				"400": problemRef(),
				"401": problemRef(),
				"403": problemRef(),
				"409": problemRef(),
				"422": problemRef(),
				"429": problemRef(),
			},
		},
		"parameters": []any{},
		"x-resource": lower,
	}
}

func itemPath(name, ref string) obj {
	idParam := obj{
		"name": "id", "in": "path", "required": true,
		"schema": obj{"type": "string"},
	}
	return obj{
		"parameters": []any{idParam},
		"get": obj{
			"summary":     "Get a " + name,
			"operationId": "get" + name,
			"responses": obj{
				"200": okObject(ref, "the "+name),
				"401": problemRef(),
				"403": problemRef(),
				"404": problemRef(),
				"429": problemRef(),
			},
		},
		"patch": obj{
			"summary":     "Update a " + name,
			"operationId": "update" + name,
			"parameters":  []any{idempotencyRef()},
			"requestBody": bodyRef(ref),
			"responses": obj{
				"200": okObject(ref, "the updated "+name),
				"400": problemRef(),
				"401": problemRef(),
				"403": problemRef(),
				"404": problemRef(),
				"409": problemRef(),
				"422": problemRef(),
				"429": problemRef(),
			},
		},
		"delete": obj{
			"summary":     "Soft-delete a " + name,
			"operationId": "delete" + name,
			"parameters":  []any{idempotencyRef()},
			"responses": obj{
				"204": obj{"description": "the " + name + " was archived"},
				"400": problemRef(),
				"401": problemRef(),
				"403": problemRef(),
				"404": problemRef(),
				"409": problemRef(),
				"429": problemRef(),
			},
		},
	}
}

// actionOperation documents one non-CRUD Action. The request/response bodies
// are generic objects (the action's payload shape is module-specific and not
// derived from a single metadata object); writes require an Idempotency-Key and
// advertise the standard write failure responses.
func actionOperation(a Action) obj {
	op := obj{
		"summary":     a.Summary,
		"operationId": operationID(a),
		"responses": obj{
			"200": obj{"description": a.Summary},
			"401": problemRef(),
			"403": problemRef(),
			"404": problemRef(),
			"429": problemRef(),
		},
	}
	if params := pathParams(a.Path); len(params) > 0 {
		op["parameters"] = params
	}
	if a.Write {
		op["parameters"] = append(pathParams(a.Path), idempotencyRef())
		op["requestBody"] = obj{
			"required": false,
			"content":  obj{"application/json": obj{"schema": obj{"type": "object"}}},
		}
		resp := op["responses"].(obj)
		resp["400"] = problemRef()
		resp["409"] = problemRef()
		resp["422"] = problemRef()
	}
	return op
}

// pathParams returns OpenAPI parameter objects for each {var} in an action
// path.
func pathParams(path string) []any {
	var out []any
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, obj{
				"name": strings.Trim(seg, "{}"), "in": "path", "required": true,
				"schema": obj{"type": "string"},
			})
		}
	}
	return out
}

// operationID derives a stable operationId from an action's method and path.
func operationID(a Action) string {
	id := strings.ToLower(a.Method)
	for _, seg := range strings.Split(a.Path, "/") {
		if seg == "" || seg == "api" || seg == "v1" {
			continue
		}
		seg = strings.Trim(seg, "{}")
		id += strings.ToUpper(seg[:1]) + seg[1:]
	}
	return id
}

func problemSchema() obj {
	return obj{
		"type": "object",
		"properties": obj{
			"type":     obj{"type": "string"},
			"title":    obj{"type": "string"},
			"status":   obj{"type": "integer"},
			"detail":   obj{"type": "string"},
			"instance": obj{"type": "string"},
		},
		"required": []any{"type", "title", "status"},
	}
}

func problemRef() obj     { return obj{"$ref": "#/components/responses/Problem"} }
func idempotencyRef() obj { return obj{"$ref": "#/components/parameters/IdempotencyKey"} }

func idempotencyKeyParam() obj {
	return obj{
		"name": "Idempotency-Key", "in": "header", "required": true,
		"description": "Client-generated key; a replay returns the identical result (ADR-009).",
		"schema":      obj{"type": "string"},
	}
}

func bodyRef(ref string) obj {
	return obj{
		"required": true,
		"content":  obj{"application/json": obj{"schema": obj{"$ref": ref}}},
	}
}

func okObject(ref, desc string) obj {
	return obj{
		"description": desc,
		"content":     obj{"application/json": obj{"schema": obj{"$ref": ref}}},
	}
}
