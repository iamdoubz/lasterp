// SPDX-License-Identifier: AGPL-3.0-only

package changefeed

import (
	"sync"

	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Notifier is told, after a feed append commits, that a tenant has changes
// past some cursor. It carries a bell, not the data: subscribers respond by
// reading the durable feed exactly as a cold client would.
//
// That is the whole reason a dropped notification is survivable. If the
// notification carried the change itself, losing one would lose data and the
// transport would need its own exactly-once and replay story. As a bell, a
// drop costs latency only — the next read finds the same rows, and INV-S5
// stays a property of the database rather than of a message broker.
//
// Notify must not block: it is called on the write path, after commit.
type Notifier interface {
	Notify(tenant tenancy.ID, cursor int64)
}

// NopNotifier discards notifications. It is the default for deployments with
// no live subscribers, where polling readers are the only consumers.
type NopNotifier struct{}

// Notify implements Notifier.
func (NopNotifier) Notify(tenancy.ID, int64) {}

// InProcess fans notifications out to subscribers inside this process, which
// is every subscriber in solo mode — one binary, one database, no broker to
// run (docs/02 makes NATS the multi-node transport; ADR-005 makes solo mode a
// single trusted process).
type InProcess struct {
	mu   sync.Mutex
	subs map[tenancy.ID][]chan int64
}

// NewInProcess returns an empty in-process notifier.
func NewInProcess() *InProcess {
	return &InProcess{subs: map[tenancy.ID][]chan int64{}}
}

// Subscribe returns a channel receiving the cursor high-water mark for tenant,
// and a function that unsubscribes and closes it.
//
// The channel is buffered depth 1 and Notify drops rather than blocks when it
// is full: a subscriber that is behind does not need to be told twice that
// there is work, it needs to catch up, and the cursor it reads on waking is
// the current one either way.
func (n *InProcess) Subscribe(tenant tenancy.ID) (<-chan int64, func()) {
	ch := make(chan int64, 1)

	n.mu.Lock()
	n.subs[tenant] = append(n.subs[tenant], ch)
	n.mu.Unlock()

	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, existing := range n.subs[tenant] {
			if existing == ch {
				n.subs[tenant] = append(n.subs[tenant][:i], n.subs[tenant][i+1:]...)
				close(ch)
				return
			}
		}
	}
}

// Notify implements Notifier.
func (n *InProcess) Notify(tenant tenancy.ID, cursor int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.subs[tenant] {
		select {
		case ch <- cursor:
		default: // already pending; the subscriber will read the latest cursor anyway
		}
	}
}
