// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/iamdoubz/lasterp/kernel/metadata"
	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// Period statuses. Close is monotonic (open → closed); reopen is a privileged,
// audited transition back (INV-F3).
const (
	PeriodOpen   = "open"
	PeriodClosed = "closed"
)

var (
	// ErrPeriodNotFound is returned when a period code is unknown to the tenant.
	ErrPeriodNotFound = errors.New("ledger: period not found")
	// ErrClosedPeriod is returned when posting targets a closed period (INV-F3).
	ErrClosedPeriod = errors.New("ledger: period is closed")
	// ErrPeriodNotOpen is returned by Close when the period is not currently open.
	ErrPeriodNotOpen = errors.New("ledger: period is not open")
	// ErrPeriodNotClosed is returned by Reopen when the period is not closed.
	ErrPeriodNotClosed = errors.New("ledger: period is not closed")
)

// CreatePeriod adds a fiscal period, open by default.
func CreatePeriod(ctx context.Context, db *storage.DB, tenant tenancy.ID, code, startDate, endDate string) (metadata.Record, error) {
	crud, err := periodCRUD()
	if err != nil {
		return nil, err
	}
	return crud.Create(ctx, db, tenant, metadata.Record{
		"code": code, "start_date": startDate, "end_date": endDate, "status": PeriodOpen,
	})
}

// ClosePeriod flips an open period to closed (monotonic). The CRUD update
// records the transition in audit_log. It errors if the period is not open, so
// close is idempotency-safe against a double request.
func ClosePeriod(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	return transitionStatus(ctx, db, tenant, id, PeriodOpen, PeriodClosed, ErrPeriodNotOpen)
}

// ReopenPeriod flips a closed period back to open — a privileged, audited
// transition (INV-F3: reopen is the reversible event, not a silent edit).
func ReopenPeriod(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) error {
	return transitionStatus(ctx, db, tenant, id, PeriodClosed, PeriodOpen, ErrPeriodNotClosed)
}

// GetPeriod reads a period record by id (authorized as Period "read").
func GetPeriod(ctx context.Context, db *storage.DB, tenant tenancy.ID, id string) (metadata.Record, error) {
	crud, err := periodCRUD()
	if err != nil {
		return nil, err
	}
	return crud.Get(ctx, db, tenant, id)
}

func transitionStatus(ctx context.Context, db *storage.DB, tenant tenancy.ID, id, from, to string, wrongState error) error {
	crud, err := periodCRUD()
	if err != nil {
		return err
	}
	current, err := crud.Get(ctx, db, tenant, id)
	if err != nil {
		return err
	}
	if s, _ := current["status"].(string); s != from {
		return fmt.Errorf("%w (status %v)", wrongState, current["status"])
	}
	_, err = crud.Update(ctx, db, tenant, id, metadata.Record{"status": to})
	return err
}

// periodStatusByCode returns a period's status, or ErrPeriodNotFound. It reads
// the generated table directly (see accountActive for why).
func periodStatusByCode(ctx context.Context, tx txQuerier, db *storage.DB, tenant tenancy.ID, code string) (string, error) {
	var status string
	row := tx.QueryRowContext(ctx, db.Rebind(
		`SELECT status FROM `+metadata.TableName(ObjectPeriod)+` WHERE tenant_id = ? AND code = ? AND archived_at IS NULL`),
		string(tenant), code)
	err := row.Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrPeriodNotFound, code)
	}
	if err != nil {
		return "", err
	}
	return status, nil
}

// Period is a fiscal period as reporting reads it: the code entries post
// against, and the dates that order one period against another.
type Period struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Status    string `json:"status"`
}

// ListPeriods returns the tenant's periods oldest first, ordered by start_date
// (then code, so periods sharing a start date still order deterministically).
//
// Chronological order is what makes "the prior period" a fact rather than a
// guess: period codes are free text, so sorting by code would put "2026-1"
// after "2026-10". Dates are stored as YYYY-MM-DD text, which sorts correctly
// as a string on both dialects (the WP-1.1 as_of precedent).
func ListPeriods(ctx context.Context, db *storage.DB, tenant tenancy.ID) ([]Period, error) {
	crud, err := periodCRUD()
	if err != nil {
		return nil, err
	}
	records, err := crud.List(ctx, db, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]Period, 0, len(records))
	for _, rec := range records {
		out = append(out, Period{
			ID:        asString(rec["id"]),
			Code:      asString(rec["code"]),
			StartDate: asString(rec["start_date"]),
			EndDate:   asString(rec["end_date"]),
			Status:    asString(rec["status"]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartDate != out[j].StartDate {
			return out[i].StartDate < out[j].StartDate
		}
		return out[i].Code < out[j].Code
	})
	return out, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
