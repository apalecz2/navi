package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/stats"
	"github.com/aidenpaleczny/navi/internal/store"
)

// Stats is what the three statistics routes call. *stats.Service is the one
// implementation and the agent's get_stats tool holds the same type, which is
// how V6 holds: every method returns a typed value this package serialises as it
// is. There is nothing here to compute with - no chain, no occurrence, no
// helper that counts - so a handler that tried would have to reach past its own
// interface.
type Stats interface {
	Summary(ctx context.Context, q stats.Query) (stats.Summary, error)
	Timeseries(ctx context.Context, q stats.Query) (stats.Timeseries, error)
	Heatmap(ctx context.Context, q stats.Query) (stats.Heatmap, error)
	GoalProgress(ctx context.Context, id string) (stats.GoalSeries, error)
}

// serveStats is the whole of each route: validate the three query parameters
// through stats.NewQuery (the same validation get_stats gets), call, serialise.
// A *domain.ValidationError from either step is a 400 with the rule named, as it
// is on every other read in this package.
func serveStats[T any](s *Server, name string, call func(context.Context, stats.Query) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query()
		q, ve := stats.NewQuery(p.Get("range"), p.Get("item_id"), p.Get("bucket"))
		if ve != nil {
			writeValidationError(w, s.log, ve)
			return
		}

		out, err := call(r.Context(), q)
		var ve2 *domain.ValidationError
		if errors.As(err, &ve2) {
			writeValidationError(w, s.log, ve2)
			return
		}
		if err != nil {
			s.log.Error("stats: "+name, "err", err)
			writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
				Error: "internal", Message: "statistics could not be loaded",
			})
			return
		}
		writeJSON(w, s.log, http.StatusOK, out)
	}
}

// handleGoalProgress is GET /api/goals/{id}/progress: one GoalSeries shape for
// both goal kinds. Like the statistics routes it serialises what the service
// returns; which kind of goal it is, is the service's business.
func (s *Server) handleGoalProgress(svc Stats) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := svc.GoalProgress(r.Context(), r.PathValue("id"))
		var ve *domain.ValidationError
		switch {
		case errors.As(err, &ve):
			writeValidationError(w, s.log, ve)
		case errors.Is(err, store.ErrNotFound):
			writeJSON(w, s.log, http.StatusNotFound, errorBody{Error: "not_found", Message: "no such goal"})
		case err != nil:
			s.log.Error("stats: goal progress", "err", err)
			writeJSON(w, s.log, http.StatusInternalServerError, errorBody{
				Error: "internal", Message: "goal progress could not be loaded",
			})
		default:
			writeJSON(w, s.log, http.StatusOK, out)
		}
	}
}
