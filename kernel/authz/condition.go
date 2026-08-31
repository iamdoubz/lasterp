// SPDX-License-Identifier: AGPL-3.0-only

package authz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/iamdoubz/lasterp/kernel/expr"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// The CRUD verbs. A conditional grant is honoured on exactly these, because
// they are the actions whose gate has a record to judge (see
// ConditionalActions).
const (
	ActionCreate = "create"
	ActionRead   = "read"
	ActionUpdate = "update"
	ActionDelete = "delete"
)

// ConditionalActions is the closed set of actions a condition may be attached
// to. Everything else is refused at grant time rather than stored and ignored:
// a rule an administrator wrote, that the gate never consults, is a permission
// that reads as narrower than it is.
//
// `read` is absent deliberately. A read gate answers object-level questions —
// GrantedObjects feeds WP-2.4's sync scope, and CRUD.List returns a page — so
// honouring a row condition there means pushing it into the query or filtering
// per row, with a paging story for the rows it rejects. That is a WP, not a
// branch here (docs/notes/WP-3.3-decisions.md §3).
var ConditionalActions = map[string]bool{
	ActionCreate: true,
	ActionUpdate: true,
	ActionDelete: true,
}

// ErrConditionUnsupportedAction is returned by GrantPermission for a condition
// on an action outside ConditionalActions.
var ErrConditionUnsupportedAction = errors.New("authz: conditions are not evaluated on this action")

// ErrConditionInvalid is returned when a condition does not compile, or does
// not evaluate to a boolean. Refused at grant time so a mistyped rule fails
// where the administrator can see it, rather than denying every request
// forever in silence.
var ErrConditionInvalid = errors.New("authz: condition is not a valid boolean CEL expression")

// Grant is one row of role_permissions as the gate sees it.
type Grant struct {
	Object string
	Action string
	// Condition is a CEL expression, or "" for an unconditional grant.
	Condition string
}

// GrantSet is every grant an actor holds for one (object, action), plus the
// actor facts a condition may read. It is the two-stage gate's carrier:
// AuthorizeGrants establishes that the actor has *some* grant, and Allow
// decides — later, once the record exists — whether it covers that record.
//
// Splitting the decision is what keeps INV-T2's ordering intact. A caller with
// no grant at all is refused before any validation runs, so a 422 naming legal
// values is never a validity oracle for someone who cannot write here at all
// (kernel/metadata/crud.go). A caller who does hold a grant is a legitimate
// user of the object; the condition then decides which rows.
type GrantSet struct {
	grants []Grant
	actor  expr.Actor
}

// Any reports whether the actor holds any grant for the (object, action).
func (g GrantSet) Any() bool { return len(g.grants) > 0 }

// Unconditional reports whether at least one held grant carries no condition.
func (g GrantSet) Unconditional() bool {
	for _, gr := range g.grants {
		if gr.Condition == "" {
			return true
		}
	}
	return false
}

// Allow decides the access against one record.
//
// **This is where INV-T3's narrowing lives, and it is structural.** Allow can
// only ever return true for a grant that is already in the set, so no condition
// can produce an allow where no grant exists. And every failure mode — false,
// an evaluation error, a cost overrun, a missing binding, a non-boolean result,
// an expression that no longer compiles — denies that grant. There is no path
// on which an unevaluable condition degrades to "allow", which is why the
// narrowing property does not depend on the evaluator being correct
// (docs/notes/WP-3.3-decisions.md §2).
func (g GrantSet) Allow(record map[string]any) bool {
	for _, gr := range g.grants {
		if gr.Condition == "" {
			return true
		}
		prg, err := expr.Get(gr.Condition)
		if err != nil {
			continue
		}
		ok, err := prg.Eval(record, g.actor)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// AuthorizeGrants is the first stage of the record-bearing gate: it requires an
// attributable actor (INV-T4) and at least one grant for (object, action)
// (INV-T2), and returns the grant set for the caller to Allow against once the
// record is in hand.
//
// A caller that takes the GrantSet and never calls Allow has silently widened
// every conditional grant to unconditional. TestEveryGrantSetIsChecked asserts
// that no call site does.
func AuthorizeGrants(ctx context.Context, db *storage.DB, object, action string) (Actor, GrantSet, error) {
	actor, err := ActorFromContext(ctx)
	if err != nil {
		return Actor{}, GrantSet{}, err
	}
	set, err := GrantsFor(ctx, db, actor, object, action)
	if err != nil {
		return Actor{}, GrantSet{}, err
	}
	if !set.Any() {
		return Actor{}, GrantSet{}, ErrPermissionDenied
	}
	return actor, set, nil
}

// AuthorizeRecord is both stages at once, for a caller that already holds the
// record its gate is about — a create, or a module acting on a loaded row.
func AuthorizeRecord(ctx context.Context, db *storage.DB, object, action string, record map[string]any) (Actor, error) {
	actor, set, err := AuthorizeGrants(ctx, db, object, action)
	if err != nil {
		return Actor{}, err
	}
	if !set.Allow(record) {
		return Actor{}, ErrPermissionDenied
	}
	return actor, nil
}

// GrantsFor loads every grant actor holds for (object, action), together with
// the actor's role names, in one tenant transaction.
//
// The roles come from the same transaction as the grants on purpose: a
// condition reading actor.roles and a gate reading the grants must see one
// consistent picture of who the actor is.
func GrantsFor(ctx context.Context, db *storage.DB, actor Actor, object, action string) (GrantSet, error) {
	if !actor.valid() {
		return GrantSet{}, ErrNoActor
	}
	if object == "" || action == "" {
		return GrantSet{}, errors.New("authz: object and action are required")
	}
	set := GrantSet{actor: expr.Actor{ID: string(actor.UserID), Tenant: string(actor.TenantID)}}
	err := tenancy.WithTenant(ctx, db, actor.TenantID, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT rp.condition FROM role_permissions rp
			JOIN user_roles ur ON ur.role_id = rp.role_id AND ur.tenant_id = rp.tenant_id
			WHERE rp.tenant_id = ? AND ur.user_id = ? AND rp.object = ? AND rp.action = ?`),
			string(actor.TenantID), string(actor.UserID), object, action)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var cond sql.NullString
			if err := rows.Scan(&cond); err != nil {
				return err
			}
			set.grants = append(set.grants, Grant{Object: object, Action: action, Condition: cond.String})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !set.needsRoles() {
			return nil
		}
		roleRows, err := tx.QueryContext(ctx, db.Rebind(`
			SELECT r.name FROM roles r
			JOIN user_roles ur ON ur.role_id = r.id AND ur.tenant_id = r.tenant_id
			WHERE r.tenant_id = ? AND ur.user_id = ?
			ORDER BY r.name`),
			string(actor.TenantID), string(actor.UserID))
		if err != nil {
			return err
		}
		defer func() { _ = roleRows.Close() }()
		for roleRows.Next() {
			var name string
			if err := roleRows.Scan(&name); err != nil {
				return err
			}
			set.actor.Roles = append(set.actor.Roles, name)
		}
		return roleRows.Err()
	})
	if err != nil {
		return GrantSet{}, fmt.Errorf("authz: load grants: %w", err)
	}
	return set, nil
}

// needsRoles reports whether any held grant carries a condition. The role query
// is skipped otherwise: the overwhelmingly common case is a set of
// unconditional grants, and that case must cost exactly what it cost before
// conditions existed (docs/09 p95).
func (g GrantSet) needsRoles() bool {
	for _, gr := range g.grants {
		if gr.Condition != "" {
			return true
		}
	}
	return false
}

// validateCondition is GrantPermission's guard. Empty is always fine — that is
// an unconditional grant, and the only kind that existed before WP-3.3a.
func validateCondition(action, condition string) error {
	if condition == "" {
		return nil
	}
	if !ConditionalActions[action] {
		owner := "a WP that wires that action's record-bearing gate"
		if action == ActionRead {
			owner = "the WP that lands row-level filtering of list and sync reads"
		}
		return fmt.Errorf("%w: %q — it lands with %s. Refusing rather than storing a rule the gate would never consult",
			ErrConditionUnsupportedAction, action, owner)
	}
	if _, err := expr.Compile(condition); err != nil {
		return fmt.Errorf("%w: %w", ErrConditionInvalid, err)
	}
	return nil
}
