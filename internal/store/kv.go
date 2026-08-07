package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// Keys in the kv table. They are constants here rather than string literals at
// each use, because a typo in a key is a silently absent value and not an
// error.
const (
	// KeyLastMaterializedThrough is how far ahead occurrences exist. The
	// sweeper reads it to detect a missed nightly run, and /healthz reports the
	// distance from now as horizon_days.
	//
	// It holds an ISO-8601 UTC instant in domain.TimeLayout, not a date. That
	// makes both readers subtraction rather than calendar arithmetic, and it
	// means neither needs a timezone to answer "how many days of runway is
	// there". Session 5 writes it; nothing writes it yet, and the absence is
	// reported as absent rather than as a horizon of zero.
	KeyLastMaterializedThrough = "last_materialized_through"

	// KeyCurrentTZ is the device timezone, which drives floating items.
	KeyCurrentTZ = "current_tz"

	// KeyLastReconcileDate prevents a duplicate check-in after a restart.
	KeyLastReconcileDate = "last_reconcile_date"

	// KeyGlobalPauseUntil is vacation mode (I6).
	KeyGlobalPauseUntil = "global_pause_until"
)

// getKV returns a raw value and whether it was present. Absence is not an
// error: every key in this table is optional state that has a defined meaning
// when missing.
func (s *Store) getKV(ctx context.Context, key string) (string, bool, error) {
	value, err := s.read.GetKV(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get %s: %w", key, err)
	}
	return value, true, nil
}

// setKV writes a raw value.
func (s *Store) setKV(ctx context.Context, key, value string) error {
	err := s.tx(ctx, func(q *sqlc.Queries) error {
		return q.SetKV(ctx, sqlc.SetKVParams{
			Key:       key,
			Value:     value,
			UpdatedAt: domain.FormatTime(time.Now()),
		})
	})
	if err != nil {
		return fmt.Errorf("store: set %s: %w", key, err)
	}
	return nil
}

// SchemaVersion returns the applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	return currentVersion(ctx, s.reader)
}

// LastMaterializedThrough returns the end of the materialization horizon and
// whether it has ever been set. A database that has never materialized reports
// false, which /healthz renders as a null horizon rather than as zero days.
func (s *Store) LastMaterializedThrough(ctx context.Context) (time.Time, bool, error) {
	value, ok, err := s.getKV(ctx, KeyLastMaterializedThrough)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	t, err := domain.ParseTime(value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: %s: %w", KeyLastMaterializedThrough, err)
	}
	return t, true, nil
}

// SetLastMaterializedThrough records how far ahead occurrences now exist.
func (s *Store) SetLastMaterializedThrough(ctx context.Context, through time.Time) error {
	return s.setKV(ctx, KeyLastMaterializedThrough, domain.FormatTime(through))
}

// Horizon is how much runway the materializer has left, in whole days, and
// whether anything has ever been materialized.
//
// It lives here for the same reason OverdueGrace does: /healthz and the
// navi_materializer_horizon_days gauge both report it, the sweeper acts on it,
// and three callers computing the same subtraction is three places for it to
// drift. Absent is not zero — a database that has never materialized has no
// horizon, and reporting that as zero days would be indistinguishable from a
// horizon that has run out, which is the one state the number exists to catch.
func (s *Store) Horizon(ctx context.Context) (int, bool, error) {
	through, ok, err := s.LastMaterializedThrough(ctx)
	if err != nil || !ok {
		return 0, false, err
	}
	return HorizonDays(through, time.Now()), true, nil
}

// HorizonDays is how many whole days of occurrences exist ahead of now. A
// horizon in the past is zero rather than negative: the number is "how much
// runway is left", and there is no such thing as less than none.
//
// Whole days, so a healthy system running a 30-day horizon reads 29 rather than
// flickering between 29 and 30 on the second the run finished. Truncating also
// errs the safe way: it under-reports the runway and never over-reports it,
// which is the direction an alerting threshold wants to be wrong in.
func HorizonDays(through, now time.Time) int {
	days := int(through.Sub(now) / (24 * time.Hour))
	if days < 0 {
		return 0
	}
	return days
}

// GlobalPauseUntil is vacation mode (I6): no occurrence is generated, notified,
// or reconciled before it. Absent means not paused.
//
// It is global rather than per item because "I'm away until Monday" is one
// statement, and resolving it into eighteen individual skips produces both worse
// statistics and a worse conversation.
func (s *Store) GlobalPauseUntil(ctx context.Context) (time.Time, bool, error) {
	value, ok, err := s.getKV(ctx, KeyGlobalPauseUntil)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	t, err := domain.ParseTime(value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: %s: %w", KeyGlobalPauseUntil, err)
	}
	return t, true, nil
}

// SetGlobalPauseUntil enters vacation mode. Like SetCurrentTZ it writes one row
// and nothing else: re-materializing the items whose future occurrences now fall
// inside the window is the caller's, because the caller is the one that knows
// whether it wants to wait for the answer.
func (s *Store) SetGlobalPauseUntil(ctx context.Context, until time.Time) error {
	return s.setKV(ctx, KeyGlobalPauseUntil, domain.FormatTime(until))
}

// CurrentTZ returns the device timezone and whether it has ever been set. It is
// the IANA name as stored, not a *time.Location: loading it is
// schedule.LoadLocation's job, and this package does not import the schedule
// machinery.
//
// Absent is a normal state, not an error. A database that has never been told
// where the user is falls back to the item's own zone and then to the
// deployment default, which is what schedule.Zones does with the two values.
func (s *Store) CurrentTZ(ctx context.Context) (string, bool, error) {
	return s.getKV(ctx, KeyCurrentTZ)
}

// SetCurrentTZ records where the device is (C7). Changing it is what triggers
// re-materialization of every floating item's future pending occurrences —
// which the caller does, because this writes one row and nothing else.
func (s *Store) SetCurrentTZ(ctx context.Context, tz string) error {
	return s.setKV(ctx, KeyCurrentTZ, tz)
}
