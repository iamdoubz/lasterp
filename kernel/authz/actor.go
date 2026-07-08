// SPDX-License-Identifier: AGPL-3.0-only

// Package authz is the WP-0.3 RBAC core: roles, permission grants, and the
// actor/authorize choke point every write path calls through (INV-T2,
// INV-T3, INV-T4).
package authz

import (
	"context"
	"errors"

	"github.com/iamdoubz/lasterp/kernel/identity"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Actor is the attributed principal behind a request. INV-T4: every
// mutation is attributable — actor, command, timestamp — no anonymous
// writes, including system/agent/plugin writes.
type Actor struct {
	TenantID tenancy.ID
	UserID   identity.UserID
}

func (a Actor) valid() bool { return a.TenantID != "" && a.UserID != "" }

type contextKey struct{}

// WithActor binds a authenticated actor to ctx.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, contextKey{}, a)
}

// ErrNoActor is returned when a write path is invoked without an
// authenticated principal bound to the context.
var ErrNoActor = errors.New("authz: no authenticated actor in context")

// ActorFromContext returns the actor bound by WithActor. Callers on a
// write path MUST call this and fail closed on error rather than proceed
// with a zero-value Actor.
func ActorFromContext(ctx context.Context) (Actor, error) {
	a, ok := ctx.Value(contextKey{}).(Actor)
	if !ok || !a.valid() {
		return Actor{}, ErrNoActor
	}
	return a, nil
}
