// SPDX-License-Identifier: AGPL-3.0-only

package eventstore

import (
	"fmt"
	"sync"
)

// UpcastFunc migrates one event's payload from schema version fromVersion
// to fromVersion+1. It never touches the stored row (INV-E3: events are
// immutable post-commit) — it only transforms what LoadStream returns.
type UpcastFunc func(payload []byte) ([]byte, error)

type upcastKey struct {
	eventType   EventType
	fromVersion int
}

// Upcasters is a registry of UpcastFunc, keyed by (event type, source
// schema version). A caller with no upcasters to apply can pass a nil
// *Upcasters to LoadStream.
type Upcasters struct {
	mu  sync.RWMutex
	fns map[upcastKey]UpcastFunc
}

// NewUpcasters returns an empty registry.
func NewUpcasters() *Upcasters {
	return &Upcasters{fns: make(map[upcastKey]UpcastFunc)}
}

// Register adds fn as the upcaster from fromVersion to fromVersion+1 for
// eventType.
func (u *Upcasters) Register(eventType EventType, fromVersion int, fn UpcastFunc) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fns[upcastKey{eventType, fromVersion}] = fn
}

// Apply repeatedly upcasts ev's payload — v1→v2→v3→... — until no
// upcaster is registered for its current schema version, then returns the
// (possibly transformed) event with schema_version updated to match.
func (u *Upcasters) Apply(ev Event) (Event, error) {
	if u == nil {
		return ev, nil
	}
	u.mu.RLock()
	defer u.mu.RUnlock()

	for {
		fn, ok := u.fns[upcastKey{ev.Type, ev.SchemaVersion}]
		if !ok {
			return ev, nil
		}
		payload, err := fn(ev.Payload)
		if err != nil {
			return Event{}, fmt.Errorf("eventstore: upcast %s v%d: %w", ev.Type, ev.SchemaVersion, err)
		}
		ev.Payload = payload
		ev.SchemaVersion++
	}
}
