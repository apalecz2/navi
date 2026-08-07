package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
)

// dbTimeout bounds the database work behind one /healthz request. A health
// check that hangs on the thing it is reporting on is worse than one that
// reports the thing is broken, because a restart policy can act on the second.
const dbTimeout = 2 * time.Second

// healthzResponse is the shape in docs/07-api-spec.md#get-healthz.
type healthzResponse struct {
	Status string  `json:"status"`
	DB     *string `json:"db"`

	Loops map[string]loopHealth `json:"loops"`

	// HorizonDays is null until something has materialized. A database with no
	// occurrences has no horizon, and reporting that as 0 would be
	// indistinguishable from a horizon that has run out — which is the one
	// state this field exists to catch.
	HorizonDays *int `json:"horizon_days"`

	// PendingOverdue above zero means the scheduler has stalled. See
	// store.OverdueGrace for what counts as overdue, and scheduler.ClaimFloor
	// for the other end of the window: rows older than the floor are past firing
	// and are not counted here, because a number that latches above zero is a
	// number nobody reads.
	PendingOverdue int `json:"pending_overdue"`
}

// loopHealth reports one loop. Every loop uses last_tick, including the
// materializer, which the spec example renders as last_run — they are all
// supervised loops with identical tick semantics, and one key shape is easier to
// consume than two.
type loopHealth struct {
	LastTick *string `json:"last_tick"`
	Healthy  bool    `json:"healthy"`
}

// handleHealthz answers "is it running".
//
// The loop half reads only process memory, so it keeps answering when the parts
// that can fail have failed. The database half is the exception and is bounded
// by dbTimeout: a database that cannot be reached is reported, not waited on.
//
// 503 when any loop has stalled or the database is unreachable, so a restart
// policy can act on it; the body is the same either way, and names which loop
// is at fault.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	snap := s.health.Snapshot()

	resp := healthzResponse{
		Status: "ok",
		Loops:  make(map[string]loopHealth, len(snap.Loops)),
	}
	for _, l := range snap.Loops {
		resp.Loops[l.Name] = loopHealth{LastTick: formatTick(l), Healthy: l.Healthy}
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()

	dbOK := s.checkDB(ctx, &resp)

	healthy := snap.Healthy && dbOK
	if !healthy {
		resp.Status = "degraded"
	}

	status := http.StatusOK
	if !healthy {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("healthz: encode response", "err", err)
	}
}

// checkDB fills in db, pending_overdue, and horizon_days, and reports whether
// the database answered. The counts are left at their zero values when it did
// not: a stale number is worse than an obviously absent one next to db "error".
func (s *Server) checkDB(ctx context.Context, resp *healthzResponse) bool {
	fail := func(op string, err error) bool {
		s.log.Error("healthz: "+op, "err", err)
		state := "error"
		resp.DB = &state
		return false
	}

	if err := s.store.Ping(ctx); err != nil {
		return fail("ping", err)
	}

	overdue, err := s.store.PendingOverdue(ctx, s.claimFloor)
	if err != nil {
		return fail("pending overdue", err)
	}
	resp.PendingOverdue = overdue

	days, ok, err := s.store.Horizon(ctx)
	if err != nil {
		return fail("horizon", err)
	}
	if ok {
		resp.HorizonDays = &days
	}

	state := "ok"
	resp.DB = &state
	return true
}

// formatTick renders a last-tick time as an ISO-8601 UTC string with a Z
// suffix, or null when the loop has not ticked yet.
func formatTick(l health.LoopStatus) *string {
	if l.LastTick.IsZero() {
		return nil
	}
	s := domain.FormatTime(l.LastTick)
	return &s
}
