// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iamdoubz/lasterp/kernel/authz"
	"github.com/iamdoubz/lasterp/kernel/eventstore"
	"github.com/iamdoubz/lasterp/kernel/idgen"
	"github.com/iamdoubz/lasterp/kernel/money"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// EventPosted is the event type for a posted journal entry. An entry is one
// stream with exactly this event at version 1 — immutable by construction
// (INV-F2); a correction is a new reversing entry, never an edit.
const EventPosted eventstore.EventType = "ledger.entry.posted"

// Posting validation errors (INV-F1 and structural).
var (
	ErrTooFewLines    = errors.New("ledger: an entry needs at least two lines")
	ErrLineNotXOR     = errors.New("ledger: each line must be a debit XOR a credit")
	ErrNegativeAmount = errors.New("ledger: line amounts must be non-negative")
	ErrUnbalanced     = errors.New("ledger: entry does not balance (Σdebits ≠ Σcredits)")
	ErrEntryNotFound  = errors.New("ledger: journal entry not found")
)

// txQuerier is the subset of *sql.Tx the internal reference checks use.
type txQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Line is one leg of a journal entry: a debit XOR a credit, in the entry's
// currency's minor units.
type Line struct {
	AccountID string
	Debit     int64
	Credit    int64
}

// PostCmd is a request to post a journal entry.
type PostCmd struct {
	Period    string // period code; must be open (INV-F3)
	Currency  string // ISO-4217; all lines share it (v1 single-currency entry)
	Memo      string
	Lines     []Line
	CommandID string // idempotency (INV-E4)
}

// Entry is a posted (or reversing) journal entry.
type Entry struct {
	ID              string
	Period          string
	Currency        string
	Memo            string
	ReversesEntryID string
	Lines           []Line
	Event           eventstore.Event
}

type linePayload struct {
	AccountID string `json:"account_id"`
	Debit     int64  `json:"debit"`
	Credit    int64  `json:"credit"`
}

type entryPayload struct {
	Period          string        `json:"period"`
	Currency        string        `json:"currency"`
	Memo            string        `json:"memo,omitempty"`
	ReversesEntryID string        `json:"reverses_entry_id,omitempty"`
	Lines           []linePayload `json:"lines"`
}

// Post validates and posts a journal entry: it must balance to the minor unit
// (INV-F1), every referenced account must exist, and the period must be open
// (INV-F3). The write is authorized (INV-T2) and attributed (INV-T4) through
// the JournalEntry object's "post" permission.
func Post(ctx context.Context, db *storage.DB, tenant tenancy.ID, cmd PostCmd) (Entry, error) {
	return post(ctx, db, tenant, cmd, "post", "")
}

// Reverse posts the compensating entry for entryID: the original's debits and
// credits swapped, referencing the original (INV-F2 — the original is never
// touched). Reversing into a closed period is refused (INV-F3).
func Reverse(ctx context.Context, db *storage.DB, tenant tenancy.ID, entryID, commandID string) (Entry, error) {
	orig, err := LoadEntry(ctx, db, tenant, entryID)
	if err != nil {
		return Entry{}, err
	}
	rev := make([]Line, len(orig.Lines))
	for i, l := range orig.Lines {
		rev[i] = Line{AccountID: l.AccountID, Debit: l.Credit, Credit: l.Debit}
	}
	cmd := PostCmd{
		Period:    orig.Period,
		Currency:  orig.Currency,
		Memo:      "reversal of " + entryID,
		Lines:     rev,
		CommandID: commandID,
	}
	return post(ctx, db, tenant, cmd, "reverse", entryID)
}

func post(ctx context.Context, db *storage.DB, tenant tenancy.ID, cmd PostCmd, action, reversesID string) (Entry, error) {
	if err := validateEntry(cmd); err != nil {
		return Entry{}, err
	}
	if err := validateRefs(ctx, db, tenant, cmd); err != nil {
		return Entry{}, err
	}

	lines := make([]linePayload, len(cmd.Lines))
	for i, l := range cmd.Lines {
		lines[i] = linePayload(l)
	}
	payload, err := json.Marshal(entryPayload{
		Period: cmd.Period, Currency: cmd.Currency, Memo: cmd.Memo,
		ReversesEntryID: reversesID, Lines: lines,
	})
	if err != nil {
		return Entry{}, err
	}
	entryID := idgen.New()
	occurredAt := time.Now().UTC()

	var ev eventstore.Event
	if db.Dialect == storage.Postgres {
		// Storage-enforced path (docs/19 layer 3): authorize, then post through
		// the SECURITY DEFINER ledger_post_entry, which re-checks balance +
		// open-period in the database and appends atomically. The app role has
		// no direct INSERT on events (INV-F5).
		actor, aerr := authz.Authorize(ctx, db, ObjectJournalEntry, action)
		if aerr != nil {
			return Entry{}, aerr
		}
		ev, err = postEntryPG(ctx, db, tenant, entryID, string(payload), cmd.Period, string(actor.UserID), cmd.CommandID, occurredAt)
	} else {
		// SQLite single trusted process: the Go pipeline above is the storage
		// owner; append through the event-sourced choke point (authz + append).
		es, jerr := journalES()
		if jerr != nil {
			return Entry{}, jerr
		}
		ev, err = es.Emit(ctx, db, tenant, action, eventstore.StreamID(entryID), 0, cmd.CommandID, eventstore.NewEvent{
			Type: EventPosted, SchemaVersion: 1, Payload: payload, OccurredAt: occurredAt,
		})
	}
	if err != nil {
		return Entry{}, err
	}

	// On an idempotent replay (same command_id) the write returns the *original*
	// event, whose stream is the original entry — use it, not the id we just
	// generated, so the returned entry id is exactly-once (INV-E4).
	return Entry{
		ID: string(ev.StreamID), Period: cmd.Period, Currency: cmd.Currency, Memo: cmd.Memo,
		ReversesEntryID: reversesID, Lines: cmd.Lines, Event: ev,
	}, nil
}

// postEntryPG calls the SECURITY DEFINER ledger_post_entry and reconstructs the
// committed (or, on replay, the original) event from what it returns.
func postEntryPG(ctx context.Context, db *storage.DB, tenant tenancy.ID, stream, payloadJSON, period, actorID, commandID string, occurredAt time.Time) (eventstore.Event, error) {
	var ev eventstore.Event
	err := tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		var id int64
		var outStream string
		row := tx.QueryRowContext(ctx, db.Rebind(
			`SELECT out_id, out_stream FROM ledger_post_entry(?, ?, ?, ?, ?, ?, ?)`),
			stream, period, payloadJSON, actorID, commandID, occurredAt, time.Now().UTC())
		if err := row.Scan(&id, &outStream); err != nil {
			return err
		}
		ev = eventstore.Event{
			ID: id, TenantID: tenant, StreamID: eventstore.StreamID(outStream), Version: 1,
			Type: EventPosted, SchemaVersion: 1, Payload: json.RawMessage(payloadJSON),
			ActorID: actorID, CommandID: commandID, OccurredAt: occurredAt,
		}
		return nil
	})
	return ev, err
}

// validateEntry is the pure (no-DB) balance and structural check (INV-F1).
func validateEntry(cmd PostCmd) error {
	if cmd.CommandID == "" {
		return errors.New("ledger: command id is required")
	}
	if cmd.Period == "" {
		return errors.New("ledger: period is required")
	}
	if _, err := money.Lookup(cmd.Currency); err != nil {
		return err
	}
	if len(cmd.Lines) < 2 {
		return ErrTooFewLines
	}
	var debits, credits int64
	for _, l := range cmd.Lines {
		if l.AccountID == "" {
			return errors.New("ledger: line account is required")
		}
		if l.Debit < 0 || l.Credit < 0 {
			return ErrNegativeAmount
		}
		if (l.Debit == 0) == (l.Credit == 0) {
			return ErrLineNotXOR // both zero or both non-zero
		}
		debits += l.Debit
		credits += l.Credit
	}
	if debits != credits {
		return ErrUnbalanced
	}
	return nil
}

// validateRefs confirms the period is open and every account exists.
//
// ponytail: check-then-append TOCTOU — the period could close between this
// read and the event append (separate transactions), since eventstore.Append
// owns its own tx. Acceptable for the pipeline-enforced PR-A; PR-B's atomic
// SECURITY DEFINER ledger_post_entry closes the window in the database.
func validateRefs(ctx context.Context, db *storage.DB, tenant tenancy.ID, cmd PostCmd) error {
	return tenancy.WithTenant(ctx, db, tenant, func(ctx context.Context, tx *sql.Tx) error {
		status, err := periodStatusByCode(ctx, tx, db, tenant, cmd.Period)
		if err != nil {
			return err
		}
		if status != PeriodOpen {
			return fmt.Errorf("%w: %q", ErrClosedPeriod, cmd.Period)
		}
		for _, l := range cmd.Lines {
			active, err := accountActive(ctx, tx, db, tenant, l.AccountID)
			if err != nil {
				return err
			}
			if !active {
				return fmt.Errorf("%w: %q", ErrAccountNotFound, l.AccountID)
			}
		}
		return nil
	})
}

// LoadEntry reads a posted entry back from its event stream.
func LoadEntry(ctx context.Context, db *storage.DB, tenant tenancy.ID, entryID string) (Entry, error) {
	_, events, err := eventstore.LoadStream(ctx, db, tenant, eventstore.StreamID(entryID), nil)
	if err != nil {
		return Entry{}, err
	}
	if len(events) == 0 {
		return Entry{}, fmt.Errorf("%w: %q", ErrEntryNotFound, entryID)
	}
	ev := events[0] // one posted event per entry stream (version 1)
	var p entryPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return Entry{}, fmt.Errorf("ledger: decode entry %q: %w", entryID, err)
	}
	lines := make([]Line, len(p.Lines))
	for i, l := range p.Lines {
		lines[i] = Line(l)
	}
	return Entry{
		ID: entryID, Period: p.Period, Currency: p.Currency, Memo: p.Memo,
		ReversesEntryID: p.ReversesEntryID, Lines: lines, Event: ev,
	}, nil
}
