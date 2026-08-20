// Command navi is the whole service: an HTTP API and five background loops in
// one process, in one container (D-019).
//
// This file is wiring and nothing else. It loads configuration, builds the
// logger and the two registries, registers the loops, starts the server, and
// shuts everything down in the order D12 requires. Anything that makes a
// decision belongs in a package.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	// A scratch image has no /usr/share/zoneinfo, and every schedule in this
	// system resolves against an IANA zone. Embedding the database is what keeps
	// a missing tzdb from looking like correct behaviour until the first DST
	// boundary.
	_ "time/tzdata"

	"github.com/aidenpaleczny/navi/internal/agent"
	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/conversation"
	"github.com/aidenpaleczny/navi/internal/copywriter"
	"github.com/aidenpaleczny/navi/internal/defaults"
	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/health"
	"github.com/aidenpaleczny/navi/internal/httpapi"
	"github.com/aidenpaleczny/navi/internal/materializer"
	"github.com/aidenpaleczny/navi/internal/metrics"
	"github.com/aidenpaleczny/navi/internal/model"
	"github.com/aidenpaleczny/navi/internal/reconciler"
	"github.com/aidenpaleczny/navi/internal/schedule"
	"github.com/aidenpaleczny/navi/internal/scheduler"
	"github.com/aidenpaleczny/navi/internal/store"
	"github.com/aidenpaleczny/navi/internal/supervisor"
	"github.com/aidenpaleczny/navi/internal/sweeper"
	"github.com/aidenpaleczny/navi/internal/transport/logging"
	"github.com/aidenpaleczny/navi/internal/transport/telegram"
)

// shutdownTimeout bounds each phase of shutdown: draining HTTP, then joining
// the loops. Both are expected to take milliseconds.
const shutdownTimeout = 5 * time.Second

// scrapeTimeout bounds the query behind navi_pending_overdue. A scrape must not
// be able to hold a connection longer than the interval between scrapes.
const scrapeTimeout = 2 * time.Second

func main() {
	if err := run(); err != nil {
		// Configuration failures happen before the logger exists, and a
		// container that exits during startup leaves nothing behind but this
		// line, so it names the variable and the reason.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.Log.Level}))
	slog.SetDefault(log)
	log.Info("starting", "config", cfg)

	// The vocabulary table is read once, here, and handed to whatever needs it
	// (D-016). Reading it before the store means a typo in defaults.yaml is a
	// failed boot with a line naming the offending key, rather than a rejected
	// reminder three days later. Nothing consumes it until P1 wires the
	// validator into the create path and the same value into the system prompt.
	table, err := defaults.Load(cfg.Files.DefaultsPath())
	if err != nil {
		return err
	}
	if err := schedule.CheckTable(table); err != nil {
		return fmt.Errorf("config: %s: %w", cfg.Files.DefaultsPath(), err)
	}
	log.Info("vocabulary defaults loaded", "path", cfg.Files.DefaultsPath(), "table", table.Summary())

	h := health.New()
	m := metrics.New()

	// The store opens before anything that could use it, and a failure here is
	// fatal: this service is the database, and a process that starts without
	// one would report healthy loops that cannot do any work. Opening also
	// applies any pending migrations.
	st, err := store.Open(context.Background(), cfg.Data, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("store close", "err", err)
		}
	}()

	notifier, err := notifyTransport(cfg.Transport.Notify, cfg.Telegram, log)
	if err != nil {
		return err
	}
	if notifier.Name() == config.LoggingTransport {
		// The one misconfiguration that looks exactly like a working system:
		// loops tick, rows are claimed and marked notified, /healthz is green,
		// and no phone ever buzzes. It is the right default for `go run` and
		// wrong in a container, so it says so every boot.
		log.Warn("notifications are going to the log, not to a device",
			"notify_transport", notifier.Name())
	}

	// The sweeper backfills the horizon by calling the materializer, so it holds
	// the same instance the supervisor drives rather than one of its own.
	// Built before chatTransport, which needs it too (the conversation
	// loop's agent.Tools re-materializes on every write).
	mat := materializer.New(log.With("loop", materializer.Name), st, cfg.Schedule.DefaultTZ)

	chatWebhook, chatIntake, err := chatTransport(cfg.Transport.Chat, cfg.Telegram,
		cfg.Model.Provider, cfg.Model.APIKey, cfg.Files.ModelRoutingPath(), cfg.Files.PersonaPath(),
		st, mat, table, cfg.Schedule.DefaultTZ, m, log)
	if err != nil {
		return err
	}
	if chatWebhook != nil {
		// Zero-value on start (RegisterLoop's argument): a webhook that has
		// never seen a message should read as "no data yet" and not be
		// missing from the series entirely.
		m.RegisterInboundAccepted(telegram.Name)
		m.RegisterInboundDropped("allowlist")
		m.RegisterInboundDropped("queue_full")
		m.RegisterInboundDropped("callback_decode")
	}

	// Loops run under their own context so shutdown can drain HTTP first and
	// cancel them second (D12).
	loopCtx, cancelLoops := context.WithCancel(context.Background())
	defer cancelLoops()

	// The scheduler's recovery window is measured back from now, which is process
	// start (C9, Q-2).
	sched := scheduler.New(log.With("loop", scheduler.Name), st, notifier, m, time.Now())

	// The claim floor is fixed for the life of the process, so the gauge, the
	// health endpoint, and the claim itself are all driven by the one value
	// rather than by three thresholds that can drift.
	claimFloor := sched.ClaimFloor()
	m.RegisterPendingOverdue(func() float64 { return pendingOverdue(st, claimFloor, log) })
	m.RegisterHorizonDays(func() float64 { return horizonDays(st, log) })
	m.RegisterTransition(string(domain.StatusPending), string(domain.StatusNotified), scheduler.Source)

	// Every edge the resolution endpoint can legally produce, so a dashboard
	// reads a zero rather than a gap before the first resolution of a given
	// shape.
	//
	// The sources are the ones that have a caller. notification joins the set in
	// the session that gave it one — the Telegram callback handler — and only
	// when a chat webhook actually exists to receive a tap, on the same rule that
	// gates the inbound counters above. sweeper still has none until
	// reconciliation, and registering it now would export a zero for something
	// nothing can send.
	resolutionSources := []domain.ResolutionSource{domain.ResolvedByWeb, domain.ResolvedByAgent}
	if chatWebhook != nil {
		resolutionSources = append(resolutionSources, domain.ResolvedByNotification)
	}

	for _, from := range []domain.Status{domain.StatusPending, domain.StatusNotified} {
		for _, to := range []domain.Status{domain.StatusCompleted, domain.StatusSkipped, domain.StatusMissed} {
			for _, src := range resolutionSources {
				m.RegisterTransition(string(from), string(to), string(src))
			}
		}
	}

	// Snooze is registered separately rather than folded into the loop above,
	// because snoozed is reachable only from notified: adding it to that `to`
	// list would export a zero for pending -> snoozed, an edge the transition
	// table forbids. Same argument the block's own comment makes about a source
	// with no caller — a series for something that cannot happen misreports the
	// system just as surely as a missing one does.
	for _, src := range resolutionSources {
		m.RegisterTransition(string(domain.StatusNotified), string(domain.StatusSnoozed), string(src))
	}

	sup := supervisor.New(log, h, m)
	loops := []supervisor.Loop{
		mat.Loop(),
		sched.Loop(),
		copywriter.New(log.With("loop", copywriter.Name)).Loop(),

		// The reconciler sends through the same notifier the scheduler does. A
		// check-in and a reminder go to the same person by the same route, and
		// there is no second adapter to choose from — which is also why a
		// Telegram outage takes out delivery and its own backstop together
		// (03-architecture's one correlated failure, accepted in D-006).
		reconciler.New(log.With("loop", reconciler.Name), st, notifier, m,
			cfg.Schedule.ReconcileAt, cfg.Schedule.DefaultTZ).Loop(),

		sweeper.New(log.With("loop", sweeper.Name), st, mat).Loop(),
	}
	if chatIntake != nil {
		loops = append(loops, chatIntake.Loop())
	}
	sup.Register(loops...)
	sup.Start(loopCtx)

	// mat again, and the same instance the supervisor and the sweeper drive:
	// the pause routes re-plan the items whose occurrences a new window
	// suppresses, and a second materializer would mean two random generators
	// drawing for the same items.
	srv := httpapi.New(cfg.HTTP, log, h, m, st, mat, claimFloor, cfg.Schedule.DefaultTZ, chatWebhook)
	serveErr := make(chan error, 1)
	go func() {
		log.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("http: serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	select {
	case err := <-serveErr:
		if err != nil {
			cancelLoops()
			sup.Wait()
			return err
		}
	case <-signalCtx.Done():
		stopSignals() // a second signal from here on kills the process outright
		log.Info("shutdown signal received")
	}

	// Drain in-flight requests first, so nothing is cut off mid-write, and only
	// then cancel the loops (D12).
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}

	cancelLoops()
	if !waitFor(sup.Wait, shutdownTimeout) {
		log.Error("loops did not stop within timeout", "timeout", shutdownTimeout)
	}

	// Reported so the goroutine-leak check after a shutdown is a log line rather
	// than a profiling session. A clean exit leaves a handful.
	log.Info("shutdown complete", "goroutines", runtime.NumGoroutine())
	return nil
}

// notifyTransport resolves NOTIFY_TRANSPORT to an adapter.
//
// An unknown name is a boot failure rather than a fallback. A typo that quietly
// degraded to the logging transport would produce a container that ticks, claims
// rows, reports healthy, and never reaches a phone — which is the failure this
// whole switch exists to make impossible.
//
// Telegram's supports_actions became true in session 15, and this switch did
// not change — the flip is a line inside the adapter, which is the entire point
// of building against scheduler.Notifier instead of a concrete type, and the
// only test D-007 gets before a second adapter exists.
func notifyTransport(name string, tg config.Telegram, log *slog.Logger) (scheduler.Notifier, error) {
	switch name {
	case config.LoggingTransport:
		return logging.New(log.With("transport", logging.Name)), nil
	case config.TelegramTransport:
		return telegram.New(tg.BotToken, tg.AllowedSenderID), nil
	default:
		return nil, fmt.Errorf("config: NOTIFY_TRANSPORT %q is not a known adapter", name)
	}
}

// chatTransport resolves CHAT_TRANSPORT to an inbound webhook handler and
// the conversation loop that drains it, or to (nil, nil) when nothing names
// it — today's default outside of P1 testing, in which case
// /webhook/telegram is never registered (internal/httpapi.New) and no
// conversation loop is ever started.
//
// An unknown non-empty name is a boot failure, same reasoning as
// notifyTransport: a typo that quietly ran with no inbound route would be a
// container that looks entirely healthy and simply never hears from anyone.
//
// Since session 11: building the webhook handler also builds the model
// client, the tool catalog, and the escalation ladder behind it — the whole
// conversational stack lives or dies with CHAT_TRANSPORT, on the same
// required-when-consumed reasoning internal/config's Model.APIKey and
// BotToken changes follow.
func chatTransport(name string, tg config.Telegram, provider, apiKey, routingPath, personaPath string,
	st *store.Store, mat *materializer.Materializer, table *defaults.Table, defaultTZ *time.Location,
	m *metrics.Metrics, log *slog.Logger) (http.Handler, *conversation.Intake, error) {
	switch name {
	case "":
		return nil, nil, nil
	case config.TelegramTransport:
		routing, err := model.LoadRouting(routingPath)
		if err != nil {
			return nil, nil, err
		}
		// MODEL_PROVIDER and config/model.yaml's base_urls are two
		// independent places to say "which provider" — this is what keeps a
		// flip of one from silently sending the other's key to the wrong
		// host (internal/model.ValidateProvider).
		if err := routing.ValidateProvider(model.Provider(provider)); err != nil {
			return nil, nil, fmt.Errorf("config: MODEL_PROVIDER %q: %w", provider, err)
		}
		client := model.New(log.With("component", "model"), routing, apiKey, st, m)
		tools := agent.New(st, mat, table, defaultTZ)
		chatSender := telegram.New(tg.BotToken, tg.AllowedSenderID)
		ladder := conversation.New(client, tools, routing, st, table, personaPath, defaultTZ, chatSender)
		intake := conversation.NewIntake(ladder)

		// chatSender is handed to the webhook as well as to the ladder: a button
		// tap is answered and its message edited on the same bot and the same
		// chat the reminder went out on, so there is nothing to construct twice.
		// st is passed as the resolver — the same *store.Store the HTTP endpoint
		// holds, reached through the same two methods, which is how a tap and a
		// web click cannot end up with different transition rules (D-014).
		inbound := telegram.NewInbound(tg.WebhookSecret, tg.AllowedSenderID,
			st, st, chatSender, intake, m, defaultTZ, log.With("transport", telegram.Name))
		return inbound, intake, nil
	default:
		return nil, nil, fmt.Errorf("config: CHAT_TRANSPORT %q is not a known adapter", name)
	}
}

// pendingOverdue answers navi_pending_overdue at scrape time. NaN rather than
// zero when the database cannot be reached, because a gap in the series is what
// "unknown" looks like on a dashboard and zero is what "healthy" looks like.
func pendingOverdue(st *store.Store, claimFloor time.Time, log *slog.Logger) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	n, err := st.PendingOverdue(ctx, claimFloor)
	if err != nil {
		log.Error("metrics: pending overdue", "err", err)
		return math.NaN()
	}
	return float64(n)
}

// horizonDays answers navi_materializer_horizon_days at scrape time. NaN both
// when the database cannot be reached and when nothing has ever been
// materialized: a database with no horizon has no number to report, and zero is
// the specific reading that means the horizon ran out.
func horizonDays(st *store.Store, log *slog.Logger) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()

	days, ok, err := st.Horizon(ctx)
	if err != nil {
		log.Error("metrics: horizon", "err", err)
		return math.NaN()
	}
	if !ok {
		return math.NaN()
	}
	return float64(days)
}

// waitFor runs wait and reports whether it returned before the timeout.
func waitFor(wait func(), timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
