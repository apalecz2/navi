// Package web serves the browser surface under /app: the day view, its static
// assets, the manifest and the service worker (D-022).
//
// Everything here is a read. The page and its fragments render what
// store.TodaysOccurrences returns and nothing else; a tap on Done, Skip or
// Snooze is a JSON POST to the /api endpoints from the browser, so the
// transition table, the 409s and the transition metric stay in exactly one
// place (D-014, invariant 4). A write handler in this package would be a second
// copy of the outcome mapping, which is why there is not one.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/store"
)

//go:embed static
var staticFS embed.FS

// Store is what the day view reads: the same two methods GET /api/today reads,
// so "what is due today" has one implementation and two renderings.
type Store interface {
	TodaysOccurrences(ctx context.Context, loc *time.Location) ([]store.TodayOccurrence, error)
	CurrentTZ(ctx context.Context) (string, bool, error)

	// The calendar's reads. ListCalendar is the one month query (V4);
	// LastMaterializedThrough is where the horizon is read from, the slot /healthz
	// reports horizon_days from; the other three open one occurrence's detail.
	ListCalendar(ctx context.Context, from, to time.Time, f store.CalendarFilter) ([]store.CalendarOccurrence, error)
	LastMaterializedThrough(ctx context.Context) (time.Time, bool, error)
	GetOccurrence(ctx context.Context, id string) (domain.Occurrence, error)
	GetItem(ctx context.Context, id string) (domain.Item, error)
	ChainFor(ctx context.Context, occurrenceID string) (store.Chain, error)
}

// App mounts the /app routes. Its fields never change after New.
type App struct {
	log       *slog.Logger
	store     Store
	defaultTZ *time.Location

	// version is a hash of every embedded static file. It is the cache key in
	// the service worker and the ?v= on every asset URL, so a rebuilt binary with
	// changed assets cannot be served a stale copy by either the HTTP cache or
	// the worker.
	version string
	assets  []string // versioned URLs the worker precaches
}

// New builds the app. defaultTZ is cfg.Schedule.DefaultTZ, the bottom rung of
// schedule.Zones, the same value the check-in and the snooze handler resolve
// against.
func New(log *slog.Logger, st Store, defaultTZ *time.Location) *App {
	a := &App{log: log, store: st, defaultTZ: defaultTZ}
	a.version, a.assets = fingerprint()
	return a
}

// fingerprint hashes the cacheable assets and lists their versioned URLs. The
// worker and the manifest are deliberately not among them: the worker must be
// byte-checked by the browser on every load, and the manifest is not a shell
// asset.
func fingerprint() (string, []string) {
	var names []string
	_ = fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasSuffix(path, "sw.js") && !strings.HasSuffix(path, "manifest.webmanifest") {
			names = append(names, path)
		}
		return nil
	})
	sort.Strings(names)

	h := sha256.New()
	urls := make([]string, 0, len(names))
	for _, n := range names {
		b, _ := staticFS.ReadFile(n)
		h.Write([]byte(n))
		h.Write(b)
		urls = append(urls, "/app/"+n)
	}
	version := hex.EncodeToString(h.Sum(nil))[:12]
	for i := range urls {
		urls[i] += "?v=" + version
	}
	return version, urls
}

// Mount registers the /app routes on mux.
func (a *App) Mount(mux *http.ServeMux) {
	// The ingress rule routes /app and /app/*; the trailing-slash form is the
	// day view and the bare form is a redirect to it, so a home-screen shortcut
	// and a typed URL land on the same page.
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /app/{$}", a.handleDay)
	mux.HandleFunc("GET /app/today", a.handleTodayFragment)
	mux.HandleFunc("GET /app/calendar", a.handleCalendar)
	mux.HandleFunc("GET /app/calendar/body", a.handleCalendarBody)
	mux.HandleFunc("GET /app/offline", a.handleOffline)
	mux.HandleFunc("GET /app/sw.js", a.handleWorker)
	mux.HandleFunc("GET /app/manifest.webmanifest", a.handleManifest)
	mux.HandleFunc("GET /app/static/", a.handleStatic)
}

// dayView loads the one thing the page and the fragment both render.
func (a *App) dayView(ctx context.Context) (Day, error) {
	zones, err := schedule.LoadZones(ctx, a.store, a.defaultTZ)
	if err != nil {
		return Day{}, fmt.Errorf("web: day view: %w", err)
	}
	loc := zones.Local()
	occs, err := a.store.TodaysOccurrences(ctx, loc)
	if err != nil {
		return Day{}, fmt.Errorf("web: day view: %w", err)
	}
	return NewDay(time.Now().In(loc), loc, occs), nil
}

// noStore marks a response as never cacheable by the browser or the worker.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func (a *App) handleDay(w http.ResponseWriter, r *http.Request) {
	day, err := a.dayView(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := Page(day, a.version).Render(r.Context(), w); err != nil {
		a.log.Error("web: render day", "err", err)
	}
}

// handleTodayFragment is what every settle swaps in. It renders the whole list
// and counts rather than one row, because a snooze mints a child row the
// client has no way to build and a counts block that disagrees with its rows is
// the worse failure.
func (a *App) handleTodayFragment(w http.ResponseWriter, r *http.Request) {
	day, err := a.dayView(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := DayBody(day).Render(r.Context(), w); err != nil {
		a.log.Error("web: render fragment", "err", err)
	}
}

// calendarView resolves m= and o= and builds the month. A malformed month or
// occurrence id is the caller's mistake and is reported as one; anything else is
// ours.
func (a *App) calendarView(r *http.Request) (Calendar, int, error) {
	zones, err := schedule.LoadZones(r.Context(), a.store, a.defaultTZ)
	if err != nil {
		return Calendar{}, http.StatusInternalServerError, fmt.Errorf("web: calendar: %w", err)
	}
	loc := zones.Local()
	month, err := parseMonth(r.URL.Query().Get("m"), loc)
	if err != nil {
		return Calendar{}, http.StatusBadRequest, err
	}
	open := r.URL.Query().Get("o")
	if open != "" && !domain.ValidID(open) {
		return Calendar{}, http.StatusBadRequest, fmt.Errorf("occurrence %q is not an occurrence id", open)
	}
	cal, err := a.buildCalendar(r.Context(), month, loc, open)
	if err != nil {
		return Calendar{}, http.StatusInternalServerError, err
	}
	return cal, http.StatusOK, nil
}

func (a *App) calendarFail(w http.ResponseWriter, status int, err error) {
	noStore(w)
	if status == http.StatusBadRequest {
		http.Error(w, err.Error(), status)
		return
	}
	a.log.Error("web: calendar", "err", err)
	http.Error(w, "the calendar could not be loaded", status)
}

func (a *App) handleCalendar(w http.ResponseWriter, r *http.Request) {
	cal, status, err := a.calendarView(r)
	if err != nil {
		a.calendarFail(w, status, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := CalendarPage(cal, a.version).Render(r.Context(), w); err != nil {
		a.log.Error("web: render calendar", "err", err)
	}
}

// handleCalendarBody is what month navigation, opening a chip and every
// resolution settle swap in: the header, the grid and the open panel together,
// so a snooze that moves a chip to another cell is never half-shown.
func (a *App) handleCalendarBody(w http.ResponseWriter, r *http.Request) {
	cal, status, err := a.calendarView(r)
	if err != nil {
		a.calendarFail(w, status, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := CalendarBody(cal).Render(r.Context(), w); err != nil {
		a.log.Error("web: render calendar body", "err", err)
	}
}

func (a *App) handleOffline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := Offline(a.version).Render(r.Context(), w); err != nil {
		a.log.Error("web: render offline", "err", err)
	}
}

func (a *App) fail(w http.ResponseWriter, err error) {
	a.log.Error("web: day view", "err", err)
	noStore(w)
	http.Error(w, "the day could not be loaded", http.StatusInternalServerError)
}

// handleStatic serves the embedded assets. They are addressed by content
// (?v=), so they are cacheable forever; the worker's cache-first rule relies on
// that same property.
func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := "static/" + strings.TrimPrefix(r.URL.Path, "/app/static/")
	b, err := staticFS.ReadFile(name)
	if err != nil || strings.HasSuffix(name, "sw.js") || strings.HasSuffix(name, "manifest.webmanifest") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

func (a *App) handleManifest(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/manifest.webmanifest")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// handleWorker serves the service worker with the cache name and precache list
// filled in. no-cache, so the browser revalidates the script on every
// navigation and a new build's worker is noticed.
func (a *App) handleWorker(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var list strings.Builder
	for i, u := range append([]string{"/app/offline"}, a.assets...) {
		if i > 0 {
			list.WriteString(", ")
		}
		fmt.Fprintf(&list, "%q", u)
	}
	out := strings.NewReplacer("__VERSION__", a.version, "__PRECACHE__", list.String()).Replace(string(b))
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(out))
}
