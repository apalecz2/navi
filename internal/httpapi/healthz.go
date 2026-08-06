package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/health"
)

// healthzResponse is the shape in docs/07-api-spec.md#get-healthz.
//
// Fields that need a database are null or zero until the session that fills
// them: db stays null, horizon_days and pending_overdue stay zero. They are
// present from the start so the response shape does not change under anything
// already parsing it.
type healthzResponse struct {
	Status         string                `json:"status"`
	DB             *string               `json:"db"`
	Loops          map[string]loopHealth `json:"loops"`
	HorizonDays    int                   `json:"horizon_days"`
	PendingOverdue int                   `json:"pending_overdue"`
}

// loopHealth reports one loop. Every loop uses last_tick, including the
// materializer, which the spec example renders as last_run — they are all
// supervised loops with identical tick semantics, and one key shape is easier to
// consume than two.
type loopHealth struct {
	LastTick *string `json:"last_tick"`
	Healthy  bool    `json:"healthy"`
}

// handleHealthz answers "is it running". It reads only process memory, so it
// keeps answering when the parts that can fail have failed.
//
// 503 when any loop has stalled, so a restart policy can act on it; the body is
// the same either way, and names which loop is at fault.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	snap := s.health.Snapshot()

	resp := healthzResponse{
		Status: "ok",
		Loops:  make(map[string]loopHealth, len(snap.Loops)),
	}
	if !snap.Healthy {
		resp.Status = "degraded"
	}
	for _, l := range snap.Loops {
		resp.Loops[l.Name] = loopHealth{LastTick: formatTick(l), Healthy: l.Healthy}
	}

	status := http.StatusOK
	if !snap.Healthy {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("healthz: encode response", "err", err)
	}
}

// formatTick renders a last-tick time as an ISO-8601 UTC string with a Z
// suffix, or null when the loop has not ticked yet.
func formatTick(l health.LoopStatus) *string {
	if l.LastTick.IsZero() {
		return nil
	}
	s := l.LastTick.UTC().Format(time.RFC3339)
	return &s
}
