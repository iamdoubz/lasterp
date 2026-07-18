// SPDX-License-Identifier: AGPL-3.0-only

// Package ledger is the M1 general-ledger module (docs/10): a chart of
// accounts and fiscal periods as CRUD objects, and journal entries as an
// event-sourced aggregate on the append-only event log (ADR-003). Posted
// entries are immutable; corrections are reversing entries (INV-F2). Every
// entry balances to the minor unit (INV-F1) and no entry posts into a closed
// period (INV-F3), enforced by the posting pipeline choke point. Modules
// import kernel/* only, never other modules (CLAUDE.md).
package ledger

import (
	"context"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
)

// Object names owned by this module (mirrored in the ledger capability
// manifest's `objects:` list).
const (
	ObjectAccount      = "Account"
	ObjectPeriod       = "Period"
	ObjectJournalEntry = "JournalEntry"
)

const accountYAML = `
object: Account
module: ledger
persistence: crud
fields:
  - {name: code, type: text, required: true, index: true}
  - {name: name, type: text, required: true}
  - {name: type, type: enum, required: true}
  - {name: parent, type: link, target: Account}
  - {name: currency, type: currency}
permissions:
  read: [ledger.viewer]
  create: [ledger.admin]
  update: [ledger.admin]
  delete: [ledger.admin]
`

// Period dates are text in v1 (display-only): posting keys off the period code
// + status, not a date range. Real date-typed fields + "which period does this
// date fall in" resolution can be added later via metadata schema evolution
// (WP-1.0a). ponytail: no date-range machinery until a caller needs it.
const periodYAML = `
object: Period
module: ledger
persistence: crud
fields:
  - {name: code, type: text, required: true, index: true}
  - {name: start_date, type: text, required: true}
  - {name: end_date, type: text, required: true}
  - {name: status, type: enum, required: true}
permissions:
  read: [ledger.viewer]
  create: [ledger.admin]
  update: [ledger.admin]
  delete: [ledger.admin]
`

// JournalEntry is event-sourced: its fields describe the shape of a posted
// entry (the projection), not a CRUD table — it gets no generated table, its
// data is the event stream. `lines` is a child table only in the projection
// sense; the posting pipeline stores the lines in the event payload.
const journalEntryYAML = `
object: JournalEntry
module: ledger
persistence: event_sourced
fields:
  - {name: period, type: text, required: true}
  - {name: currency, type: currency, required: true}
  - {name: memo, type: text}
  - {name: reverses_entry_id, type: text}
  - {name: lines, type: table, target: JournalLine}
permissions:
  read: [ledger.viewer]
  post: [ledger.poster]
  reverse: [ledger.poster]
`

func effective(yaml string) (*metadata.EffectiveSchema, error) {
	obj, err := metadata.ParseObject([]byte(yaml))
	if err != nil {
		return nil, err
	}
	return metadata.Merge(obj)
}

func accountCRUD() (*metadata.CRUD, error) {
	eff, err := effective(accountYAML)
	if err != nil {
		return nil, err
	}
	return metadata.NewCRUD(eff)
}

func periodCRUD() (*metadata.CRUD, error) {
	eff, err := effective(periodYAML)
	if err != nil {
		return nil, err
	}
	return metadata.NewCRUD(eff)
}

func journalES() (*metadata.EventSourced, error) {
	eff, err := effective(journalEntryYAML)
	if err != nil {
		return nil, err
	}
	return metadata.NewEventSourced(eff)
}

// Register persists the ledger's object schemas (core layer) and applies the
// DDL for the CRUD-backed objects (Account, Period). JournalEntry is
// event-sourced, so it is registered but gets no generated table — its data
// lives in the event log.
func Register(ctx context.Context, db *storage.DB) error {
	for _, s := range []struct {
		name string
		yaml string
		ddl  bool
	}{
		{ObjectAccount, accountYAML, true},
		{ObjectPeriod, periodYAML, true},
		{ObjectJournalEntry, journalEntryYAML, false},
	} {
		eff, err := effective(s.yaml)
		if err != nil {
			return err
		}
		if err := metadata.SaveObjectSchema(ctx, db, "", metadata.LayerCore, s.name, 1, []byte(s.yaml)); err != nil {
			return err
		}
		if s.ddl {
			if err := metadata.ApplyDDL(ctx, db, eff, 1); err != nil {
				return err
			}
		}
	}
	return nil
}
