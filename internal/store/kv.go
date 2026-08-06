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
