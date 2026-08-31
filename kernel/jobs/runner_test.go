// SPDX-License-Identifier: AGPL-3.0-only

package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iamdoubz/lasterp/kernel/storage"
	"github.com/iamdoubz/lasterp/kernel/tenancy"
)

// recorder is a handler that remembers what it was given.
type recorder struct {
	mu       sync.Mutex
	payloads []string
	fail     error
	panicOn  bool
}

func (r *recorder) handler() Handler {
	return func(ctx context.Context, tenant tenancy.ID, payload []byte) error {
		r.mu.Lock()
		r.payloads = append(r.payloads, string(payload))
		fail, boom := r.fail, r.panicOn
		r.mu.Unlock()
		if boom {
			panic("handler exploded")
		}
		return fail
	}
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.payloads...)
}

func TestRunnerRunsAndCompletes(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			rec := &recorder{}
			reg := NewRegistry()
			reg.Register("greet", rec.handler())

			for _, p := range []string{"a", "b", "c"} {
				if _, err := Enqueue(ctx, db, tenant, "greet", []byte(p), now, ""); err != nil {
					t.Fatalf("Enqueue: %v", err)
				}
			}

			ran, err := NewRunner(db, reg, "w").RunOnce(ctx, tenant, now)
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if ran != 3 {
				t.Fatalf("ran %d jobs, want 3", ran)
			}
			if got := rec.seen(); len(got) != 3 {
				t.Fatalf("handler saw %v, want three payloads", got)
			}
			// Everything completed, so a second pass has nothing to do.
			ran, err = NewRunner(db, reg, "w").RunOnce(ctx, tenant, now)
			if err != nil || ran != 0 {
				t.Fatalf("second pass ran %d jobs (err %v), want 0 — completed jobs are being re-run", ran, err)
			}
		})
	}
}

// A failing handler is the job's problem, not the pass's: the drain keeps
// going, and the job retries and then dead-letters.
func TestRunnerFailureRetriesThenDeadLetters(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			rec := &recorder{fail: errors.New("nope")}
			reg := NewRegistry()
			reg.Register("flaky", rec.handler())
			if _, err := Enqueue(ctx, db, tenant, "flaky", []byte("x"), now, ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}

			runner := NewRunner(db, reg, "w")
			for attempt := 1; attempt <= MaxAttempts; attempt++ {
				// Each pass is far enough ahead that the backoff has elapsed.
				if _, err := runner.RunOnce(ctx, tenant, now.Add(time.Duration(attempt)*time.Hour)); err != nil {
					t.Fatalf("RunOnce (attempt %d): %v", attempt, err)
				}
			}
			if got := rec.seen(); len(got) != MaxAttempts {
				t.Fatalf("handler ran %d times, want %d", len(got), MaxAttempts)
			}

			letters, err := DeadLetters(ctx, db, tenant, "flaky")
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 1 {
				t.Fatalf("got %d dead letters, want 1", len(letters))
			}
			if !strings.Contains(letters[0].Error, "nope") {
				t.Fatalf("dead letter lost the handler's cause: %q", letters[0].Error)
			}
		})
	}
}

// A panicking handler must not take down the runner — and with it every other
// tenant's queue. The panic still has to be visible, so it becomes the cause.
func TestRunnerSurvivesAPanickingHandler(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			boom := &recorder{panicOn: true}
			fine := &recorder{}
			reg := NewRegistry()
			reg.Register("boom", boom.handler())
			reg.Register("fine", fine.handler())

			if _, err := Enqueue(ctx, db, tenant, "boom", []byte("1"), now, ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if _, err := Enqueue(ctx, db, tenant, "fine", []byte("2"), now, ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}

			runner := NewRunner(db, reg, "w")
			if _, err := runner.RunOnce(ctx, tenant, now); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			// The job behind the panicking one still ran.
			if got := fine.seen(); len(got) != 1 {
				t.Fatalf("the job after the panicking one did not run: %v", got)
			}
			for attempt := 2; attempt <= MaxAttempts; attempt++ {
				if _, err := runner.RunOnce(ctx, tenant, now.Add(time.Duration(attempt)*time.Hour)); err != nil {
					t.Fatalf("RunOnce: %v", err)
				}
			}
			letters, err := DeadLetters(ctx, db, tenant, "boom")
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 1 || !strings.Contains(letters[0].Error, "panicked") {
				t.Fatalf("the panic was swallowed rather than recorded: %+v", letters)
			}
		})
	}
}

// An unregistered kind is what a rolling deploy or an uninstalled plugin looks
// like. It retries, then dead-letters naming the kind.
func TestRunnerUnknownKindDeadLetters(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			if _, err := Enqueue(ctx, db, tenant, "nobody.handles.this", nil, now, ""); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			runner := NewRunner(db, NewRegistry(), "w")
			for attempt := 1; attempt <= MaxAttempts; attempt++ {
				if _, err := runner.RunOnce(ctx, tenant, now.Add(time.Duration(attempt)*time.Hour)); err != nil {
					t.Fatalf("RunOnce: %v", err)
				}
			}
			letters, err := DeadLetters(ctx, db, tenant, "")
			if err != nil {
				t.Fatalf("DeadLetters: %v", err)
			}
			if len(letters) != 1 || !strings.Contains(letters[0].Error, "nobody.handles.this") {
				t.Fatalf("dead letter does not name the unhandled kind: %+v", letters)
			}
		})
	}
}

func TestRegistryRefusesADuplicateKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a kind twice did not panic; which handler runs would depend on init order")
		}
	}()
	reg := NewRegistry()
	reg.Register("k", func(context.Context, tenancy.ID, []byte) error { return nil })
	reg.Register("k", func(context.Context, tenancy.ID, []byte) error { return nil })
}

// --- schedules ---

func TestScheduleFiresAndAdvances(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			// A fixed instant so the cron arithmetic is exact.
			base := time.Date(2026, 8, 31, 1, 30, 0, 0, time.UTC)

			// docs/05's own example: the manifest's `schedule: ["0 2 * * *"]`.
			s := Schedule{ID: "plugin:com.acme.x:0", Kind: "plugin.run", Cron: "0 2 * * *",
				Payload: []byte(`{"fn":"nightly"}`), Owner: "plugin:com.acme.x"}
			if err := UpsertSchedule(ctx, db, tenant, s, base); err != nil {
				t.Fatalf("UpsertSchedule: %v", err)
			}

			// Not due at 01:30.
			n, err := TickSchedules(ctx, db, tenant, base)
			if err != nil {
				t.Fatalf("TickSchedules: %v", err)
			}
			if n != 0 {
				t.Fatalf("enqueued %d jobs before the schedule was due", n)
			}

			// Due at 02:00.
			at2 := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
			n, err = TickSchedules(ctx, db, tenant, at2)
			if err != nil {
				t.Fatalf("TickSchedules: %v", err)
			}
			if n != 1 {
				t.Fatalf("enqueued %d jobs at the firing time, want 1", n)
			}

			// Ticking again at the same instant must not fire it twice.
			n, err = TickSchedules(ctx, db, tenant, at2)
			if err != nil {
				t.Fatalf("TickSchedules (repeat): %v", err)
			}
			if n != 0 {
				t.Fatalf("the same firing enqueued %d more jobs; the advance is not exclusive", n)
			}

			// …and it advanced to tomorrow, not to some point today.
			list, err := ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("got %d schedules, want 1", len(list))
			}
			want := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
			if !list[0].NextRunAt.Equal(want) {
				t.Fatalf("next firing = %s, want %s", list[0].NextRunAt, want)
			}
		})
	}
}

// Two nodes ticking the same tenant at the same instant produce one job. The
// advance is a compare-and-set and the enqueue is deduplicated on the firing,
// so neither node has to know the other exists.
func TestConcurrentTicksFireOnce(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			base := time.Date(2026, 8, 31, 1, 30, 0, 0, time.UTC)
			at2 := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)

			if err := UpsertSchedule(ctx, db, tenant, Schedule{
				ID: "nightly", Kind: "k", Cron: "0 2 * * *", Owner: "o",
			}, base); err != nil {
				t.Fatalf("UpsertSchedule: %v", err)
			}

			const nodes = 6
			var wg sync.WaitGroup
			total := make([]int, nodes)
			for i := 0; i < nodes; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					n, err := TickSchedules(ctx, db, tenant, at2)
					if err != nil {
						t.Errorf("TickSchedules: %v", err)
					}
					total[i] = n
				}(i)
			}
			wg.Wait()

			sum := 0
			for _, n := range total {
				sum += n
			}
			if sum != 1 {
				t.Fatalf("%d nodes enqueued %d jobs for one firing, want 1", nodes, sum)
			}
		})
	}
}

// A deployment down over several firings owes one run, not one per missed
// occurrence: the next firing is computed forward from now.
func TestMissedWindowFiresOnce(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

			if err := UpsertSchedule(ctx, db, tenant, Schedule{
				ID: "hourly", Kind: "k", Cron: "0 * * * *", Owner: "o",
			}, base); err != nil {
				t.Fatalf("UpsertSchedule: %v", err)
			}
			// Thirty days later: 720 hourly firings were missed.
			late := base.AddDate(0, 0, 30)
			n, err := TickSchedules(ctx, db, tenant, late)
			if err != nil {
				t.Fatalf("TickSchedules: %v", err)
			}
			if n != 1 {
				t.Fatalf("a 30-day outage enqueued %d jobs, want 1 — the backlog must collapse", n)
			}
			list, err := ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if !list[0].NextRunAt.After(late) {
				t.Fatalf("next firing %s is not after %s; the schedule is still in the past", list[0].NextRunAt, late)
			}
		})
	}
}

func TestUpsertScheduleRefusesABadCron(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()
			for _, cron := range []string{"", "not a cron", "0 2 * *", "0 0 30 2 *"} {
				err := UpsertSchedule(ctx, db, tenant, Schedule{
					ID: "s", Kind: "k", Cron: cron, Owner: "o",
				}, now)
				if err == nil {
					t.Fatalf("UpsertSchedule accepted %q", cron)
				}
			}
			list, err := ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(list) != 0 {
				t.Fatalf("a refused schedule was stored anyway: %+v", list)
			}
		})
	}
}

// Uninstalling takes a plugin's schedules with it. A schedule left behind is
// work that keeps firing for code that is gone.
func TestDeleteSchedulesByOwner(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			now := time.Now().UTC()

			for _, s := range []Schedule{
				{ID: "a:0", Kind: "k", Cron: "0 2 * * *", Owner: "plugin:a"},
				{ID: "a:1", Kind: "k", Cron: "0 3 * * *", Owner: "plugin:a"},
				{ID: "b:0", Kind: "k", Cron: "0 4 * * *", Owner: "plugin:b"},
			} {
				if err := UpsertSchedule(ctx, db, tenant, s, now); err != nil {
					t.Fatalf("UpsertSchedule: %v", err)
				}
			}
			if err := DeleteSchedulesByOwner(ctx, db, tenant, "plugin:a"); err != nil {
				t.Fatalf("DeleteSchedulesByOwner: %v", err)
			}
			list, err := ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(list) != 1 || list[0].Owner != "plugin:b" {
				t.Fatalf("after deleting plugin:a's schedules, got %+v", list)
			}
		})
	}
}

// Replacing a schedule recomputes its firing from the new expression: the old
// firing is not the answer to the new question.
func TestUpsertScheduleReplacesTheExpression(t *testing.T) {
	for dialect, db := range testDialects(t) {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			tenant := mustCreateTenant(t, db)
			base := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

			s := Schedule{ID: "s", Kind: "k", Cron: "0 2 * * *", Owner: "o"}
			if err := UpsertSchedule(ctx, db, tenant, s, base); err != nil {
				t.Fatalf("UpsertSchedule: %v", err)
			}
			s.Cron = "0 5 * * *"
			if err := UpsertSchedule(ctx, db, tenant, s, base); err != nil {
				t.Fatalf("UpsertSchedule (replace): %v", err)
			}
			list, err := ListSchedules(ctx, db, tenant)
			if err != nil {
				t.Fatalf("ListSchedules: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("replacing a schedule made a second row: %+v", list)
			}
			want := time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC)
			if !list[0].NextRunAt.Equal(want) {
				t.Fatalf("next firing = %s, want %s (the new expression)", list[0].NextRunAt, want)
			}
		})
	}
}

var _ = storage.Postgres
