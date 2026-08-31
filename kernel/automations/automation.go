// SPDX-License-Identifier: AGPL-3.0-only

// Package automations is the WP-3.3b automation engine: trigger → condition →
// action, declared as data (docs/01 §Automations, docs/05's no-code row).
//
// The three pieces come from three earlier WPs and this one only joins them:
// the **trigger** rides WP-2.1's change feed with a per-automation cursor, the
// same way WP-3.1b's async hook runner does; the **condition** is WP-3.3a's
// `kernel/expr`, in the same closed CEL environment an RBAC grant uses; and an
// **action** executes through the ordinary CRUD gate and audit, or onto
// WP-3.3b's own job queue.
//
// An automation is its own principal, `automation:<id>`, never the user whose
// write triggered it — the call WP-3.1a made for plugins, for the same reason:
// an audit row must name what acted, and authority that varies by trigger
// cannot be reviewed when the automation is written (docs/notes/WP-3.3-decisions.md §5).
package automations

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/iamdoubz/lasterp/kernel/expr"
	"github.com/iamdoubz/lasterp/kernel/jobs"
)

// Trigger kinds.
const (
	TriggerObject   = "object"
	TriggerSchedule = "schedule"
)

// Action types this engine implements.
const (
	ActionFieldUpdate = "field_update"
	ActionCallPlugin  = "call_plugin"
)

// Verbs an object trigger may subscribe to.
var triggerVerbs = map[string]bool{"create": true, "update": true, "delete": true}

// idRE bounds an automation id: it becomes a principal name and an audit actor.
var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// fieldRE bounds a field name a field_update may set.
var fieldRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Definition is an automation as written.
type Definition struct {
	ID      string  `yaml:"id"`
	Name    string  `yaml:"name"`
	Trigger Trigger `yaml:"trigger"`
	// Condition is an optional CEL expression over `record` and `actor`, in the
	// same closed environment RBAC conditions use (ADR-022). Empty means the
	// automation fires on every trigger.
	Condition string   `yaml:"condition"`
	Actions   []Action `yaml:"actions"`
	// Enabled defaults to true; a disabled automation is kept and skipped,
	// because deleting one to pause it loses the definition.
	Enabled *bool `yaml:"enabled"`
}

// Trigger is what starts the automation. Exactly one of Object or Schedule.
type Trigger struct {
	// Object subscribes to a business object's changes, delivered from the
	// change feed.
	Object string `yaml:"object"`
	// On narrows an object trigger to particular verbs. Empty means all.
	//
	// Note what the feed can actually tell us: a feed entry carries no verb
	// (WP-3.1b-decisions §2), so `on:` is matched against the *source* the feed
	// records where it can and is otherwise permissive. It is a filter, never a
	// guarantee — which is why a condition is the place to express a real rule.
	On []string `yaml:"on"`
	// Schedule is a 5-field cron expression, run through the WP-3.3b queue.
	Schedule string `yaml:"schedule"`
}

// Action is one thing an automation does when it fires.
type Action struct {
	Type string `yaml:"type"`
	// Set is field_update's assignment map: field → literal value.
	//
	// Literals only, deliberately. An expression here would be a second
	// evaluation surface with a second set of bindings, and the first thing it
	// would want is the record it is about to overwrite — which is how a
	// field_update becomes a way to copy one field onto another without anyone
	// reviewing it. Computed values belong in a plugin, which is sandboxed.
	Set map[string]any `yaml:"set"`
	// Plugin and Fn are call_plugin's target.
	Plugin string `yaml:"plugin"`
	Fn     string `yaml:"fn"`
}

// deferredActions are named in docs/05's action table and are not implemented.
// Refused at parse time with their owner, never accepted and ignored: an
// administrator who writes one and gets a silent no-op has been lied to, which
// is the rule the plugin manifest already follows.
var deferredActions = map[string]string{
	"email":            "the WP that ships outbound mail — there is no mailer in the tree to build it on",
	"approval_request": "WP-3.4, which needs the approval-gate object anyway",
	"webhook":          "WP-3.3c: the audited outbound client is shaped around a plugin manifest's allowlist, and an automation needs that allowlist generalised — a design, not a line here (docs/notes/WP-3.3-decisions.md §5a)",
}

// ErrDeferredAction is returned for an action named in the docs but not built.
var ErrDeferredAction = errors.New("automations: this action is not implemented yet")

// Parse reads and validates a definition.
func Parse(data []byte) (*Definition, error) {
	var d Definition
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // an unknown key is a typo or a feature we do not have
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("automations: parse: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// Validate rejects a definition this engine cannot honour exactly.
func (d *Definition) Validate() error {
	if !idRE.MatchString(d.ID) {
		return fmt.Errorf("automations: %q is not a valid automation id", d.ID)
	}
	if d.Name == "" {
		return errors.New("automations: an automation needs a name")
	}

	object, schedule := d.Trigger.Object != "", d.Trigger.Schedule != ""
	switch {
	case object && schedule:
		return errors.New("automations: a trigger is an object or a schedule, not both")
	case !object && !schedule:
		return errors.New("automations: a trigger needs an object or a schedule")
	case schedule:
		if err := jobs.ValidCron(d.Trigger.Schedule); err != nil {
			return fmt.Errorf("automations: trigger.schedule: %w", err)
		}
		if len(d.Trigger.On) > 0 {
			return errors.New("automations: trigger.on applies to an object trigger, not a schedule")
		}
	case object:
		for _, v := range d.Trigger.On {
			if !triggerVerbs[v] {
				return fmt.Errorf("automations: trigger.on: %q is not create, update or delete", v)
			}
		}
	}

	if d.Condition != "" {
		// Compiled here, at the moment the automation is written, so a mistyped
		// rule fails where its author can see it. The alternative — discovering
		// it in the runner — is an automation that silently never fires, since
		// an unevaluable condition denies (ADR-022).
		if _, err := expr.Compile(d.Condition); err != nil {
			return fmt.Errorf("automations: condition: %w", err)
		}
	}

	if len(d.Actions) == 0 {
		return errors.New("automations: an automation with no actions would do nothing")
	}
	for i := range d.Actions {
		if err := d.Actions[i].validate(d); err != nil {
			return fmt.Errorf("automations: action %d: %w", i, err)
		}
	}
	return nil
}

func (a *Action) validate(d *Definition) error {
	if owner, deferred := deferredActions[a.Type]; deferred {
		return fmt.Errorf("%w: %q lands with %s", ErrDeferredAction, a.Type, owner)
	}
	switch a.Type {
	case ActionFieldUpdate:
		if d.Trigger.Object == "" {
			// A field_update needs a record to update, and a schedule trigger
			// has none. Refused rather than silently skipped at run time.
			return errors.New("field_update needs an object trigger; a schedule has no record to update")
		}
		if len(a.Set) == 0 {
			return errors.New("field_update needs at least one field to set")
		}
		for field := range a.Set {
			if !fieldRE.MatchString(field) {
				return fmt.Errorf("%q is not a valid field name", field)
			}
			if field == "id" || field == "tenant_id" {
				return fmt.Errorf("%q cannot be set by an automation", field)
			}
		}
	case ActionCallPlugin:
		if a.Plugin == "" || a.Fn == "" {
			return errors.New("call_plugin needs a plugin and a fn")
		}
	case "":
		return errors.New("an action needs a type")
	default:
		return fmt.Errorf("%q is not an action this engine implements", a.Type)
	}
	return nil
}

// IsEnabled reports whether the definition is active. Absent means enabled.
func (d *Definition) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// TriggerKind is "object" or "schedule".
func (d *Definition) TriggerKind() string {
	if d.Trigger.Schedule != "" {
		return TriggerSchedule
	}
	return TriggerObject
}

// Permissions is the authority this definition needs, as authz (object, action)
// tuples — the vocabulary the rest of the kernel already speaks.
//
// An automation acts as its own principal, so it needs its own grants; without
// them every read it makes is denied and the automation is inert. They are
// derived from the definition rather than declared separately, because a
// second list to keep in sync with the actions is a list that drifts.
//
// `read` is always required for an object trigger: the condition is evaluated
// against the record, so an automation that cannot read cannot decide.
func (d *Definition) Permissions() [][2]string {
	seen := map[[2]string]bool{}
	var out [][2]string
	add := func(object, action string) {
		key := [2]string{object, action}
		if object == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	if d.Trigger.Object != "" {
		add(d.Trigger.Object, "read")
	}
	for i := range d.Actions {
		switch d.Actions[i].Type {
		case ActionFieldUpdate:
			add(d.Trigger.Object, "update")
		case ActionCallPlugin:
			// Running someone else's sandboxed code is its own power, and it is
			// the same tuple a human needs to call `/ext/` (WP-3.2a).
			add("plugin", "invoke")
		}
	}
	return out
}

// PrincipalFor is the actor id an automation writes as.
//
// Deliberately parallel to plugins.PrincipalFor and deliberately not a users
// row: nothing can log in as an automation, because no login path can find a
// principal that does not exist in `users`.
func PrincipalFor(id string) string { return "automation:" + id }

// Principal is the definition's actor id.
func (d *Definition) Principal() string { return PrincipalFor(d.ID) }
