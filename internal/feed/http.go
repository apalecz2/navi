package feed

import (
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/aidenpaleczny/navi/internal/schedule"
)

// Defaults and cap for the requested range (07-api-spec, Calendar). The cap is
// the one GET /api/occurrences uses.
const (
	DefaultDaysBack    = 30
	DefaultDaysForward = 90
	MaxDays            = 400
)

// Mount registers the route. The path token is the whole of the authentication:
// calendar clients cannot do the Access PIN flow, so this route sits outside
// Access (ops/cloudflared-ingress.md).
func (f *Feed) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /calendar/{file}", f.serve)
}

func (f *Feed) serve(w http.ResponseWriter, r *http.Request) {
	// A wrong token is exactly an unknown path: the same body the mux gives a
	// route that does not exist, so nothing confirms /calendar/ is real. The
	// comparison is constant-time; the token is the only secret in the URL.
	want := []byte(f.token + ".ics")
	if subtle.ConstantTimeCompare([]byte(r.PathValue("file")), want) != 1 {
		http.NotFound(w, r)
		return
	}

	zones, err := schedule.LoadZones(r.Context(), f.st, f.defaultTZ)
	if err != nil {
		f.log.Error("feed: load zones", "err", err)
		http.Error(w, "calendar unavailable", http.StatusInternalServerError)
		return
	}
	loc := zones.Local()

	lo, hi, msg := f.parseRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), loc)
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	body, err := f.Build(r.Context(), zones, lo, hi)
	if err != nil {
		f.log.Error("feed: build", "err", err)
		http.Error(w, "calendar unavailable", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/calendar; charset=utf-8")
	h.Set("Content-Disposition", `inline; filename="navi.ics"`)
	// The URL is the credential: keep it out of caches and referrers.
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex")
	_, _ = w.Write(body)
}

// parseRange reads from and to as inclusive ISO dates in the device zone, as
// GET /api/occurrences does, and returns the half-open instant range. Day
// arithmetic is by calendar, never by adding 24 hours.
func (f *Feed) parseRange(fromS, toS string, loc *time.Location) (lo, hi time.Time, msg string) {
	today := f.now().In(loc)
	y, m, d := today.Date()

	from := time.Date(y, m, d-DefaultDaysBack, 0, 0, 0, 0, loc)
	to := time.Date(y, m, d+DefaultDaysForward, 0, 0, 0, 0, loc)
	var err error
	if fromS != "" {
		if from, err = time.ParseInLocation("2006-01-02", fromS, loc); err != nil {
			return lo, hi, "from is not an ISO date, YYYY-MM-DD"
		}
	}
	if toS != "" {
		if to, err = time.ParseInLocation("2006-01-02", toS, loc); err != nil {
			return lo, hi, "to is not an ISO date, YYYY-MM-DD"
		}
	}
	if to.Before(from) {
		return lo, hi, "from is after to"
	}
	utc := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	if int(utc(to).Sub(utc(from))/(24*time.Hour))+1 > MaxDays {
		return lo, hi, "the range is over 400 days"
	}
	return from, time.Date(to.Year(), to.Month(), to.Day()+1, 0, 0, 0, 0, loc), ""
}
