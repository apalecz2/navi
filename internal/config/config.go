// Package config loads and validates the process configuration from the
// environment. It is the only package in this repository that reads environment
// variables: everything else receives the group of settings it needs as a value,
// passed in by main.
//
// Validation is required-when-consumed. The struct carries every variable P0
// will eventually need, but Load only insists on the ones the code actually
// reads today. A later session adds its variables to the required set in the
// same commit as the component that reads them, so the process never demands a
// credential it has no use for.
//
// Nothing that signs or authorizes has a default value (D9, D10). A default
// signing key is indistinguishable from no signing key, and it is the failure
// that survives being copied to a second machine.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the whole of the process configuration, grouped by the component
// that consumes it. Components take their own group, never the whole struct.
type Config struct {
	HTTP      HTTP
	Log       Log
	Data      Data
	Files     Files
	Schedule  Schedule
	Transport Transport
	Telegram  Telegram
	Backup    Backup
}

// HTTP configures the API listener.
type HTTP struct {
	// Addr is the listen address, host:port. Defaults to :8000, which is the
	// port the tunnel ingress and the Prometheus scrape config both name.
	Addr string
}

// Log configures the slog handler.
type Log struct {
	Level slog.Level
}

// Data locates the writable state directory. The SQLite file and any secret
// generated on first run (D10) live under it.
type Data struct {
	// Dir must be on local disk: SQLite locking is unreliable over NFS and SMB
	// (D3).
	Dir string

	// DatabasePath overrides where the SQLite file lives. Empty means
	// "DATABASE_PATH was not set", which is the normal case; see DBPath.
	DatabasePath string
}

// DBPath is where the SQLite file lives: DATABASE_PATH when set, otherwise
// navi.db under Dir.
//
// Defaulting rather than requiring it means the deployment names one directory
// and gets a database, a WAL, and any generated secret in the same place, which
// is the same place Litestream replicates and the volume mounts. Setting both
// variables to disagreeing directories is the failure this avoids.
func (d Data) DBPath() string {
	if d.DatabasePath != "" {
		return d.DatabasePath
	}
	return filepath.Join(d.Dir, "navi.db")
}

// Files locates the read-only configuration mounted into the container: the
// vocabulary table and, from P5, the persona.
//
// They are files rather than environment variables because both are edited
// rather than set. Retuning the vocabulary or the voice should be a text edit
// and a restart, not a rebuild (D-016, G5), and neither is a credential.
type Files struct {
	// ConfigDir defaults to /config, which is where the compose file mounts
	// them and where the image carries its own copies, so a missing mount is a
	// stale table rather than a process that will not start.
	ConfigDir string
}

// DefaultsPath is the vocabulary table (D-016).
func (f Files) DefaultsPath() string {
	return filepath.Join(f.ConfigDir, "defaults.yaml")
}

// PersonaPath is the voice definition (G5). Nothing reads it until P5; the
// helper is here so that when something does, it does not invent a second way
// to spell the path.
func (f Files) PersonaPath() string {
	return filepath.Join(f.ConfigDir, "persona.md")
}

// Schedule holds the timezone settings the scheduling machinery resolves
// against. Per-item timezones live on the item row (C5); this is the deployment
// default used when nothing more specific applies.
type Schedule struct {
	// DefaultTZ is a validated IANA location. Loading it at startup is also the
	// check that the embedded tzdb survived the build: a scratch image has no
	// /usr/share/zoneinfo, and a missing tzdb looks like correct behaviour right
	// up until the first DST boundary.
	DefaultTZ *time.Location
}

// Transport names the adapter filling each transport role. Both point at the
// same one to start with (D-006); they stay separate because that split is the
// seam a dedicated push channel drops into later, and it costs one environment
// variable to keep against a refactor to reintroduce.
//
// Separate from Telegram because these are role assignments and not Telegram's
// credentials — the scheduler reads Notify and must not appear to be reading a
// bot token to do it.
type Transport struct {
	// Notify is the outbound half of the firing path. Consumed by the scheduler.
	Notify string

	// Chat is the conversational transport. Nothing reads it until P1.
	Chat string
}

// LoggingTransport is the adapter that delivers to the log instead of to a
// phone. It is the default for Notify so the service runs with no credentials,
// which is right for development and wrong everywhere else — main warns loudly
// when it is in force.
const LoggingTransport = "logging"

// TelegramTransport is the adapter that delivers to a real device, since
// session 6.
const TelegramTransport = "telegram"

// Telegram holds Telegram's credentials. BotToken and WebhookSecret are
// borrowed and come from the environment with no default (D9).
//
// AllowedSenderID does double duty. D8 will read it as the inbound sender
// allowlist once P1 wires conversation; since session 6 it is also the
// outbound recipient for the notify role — this is a single-user system with
// no user table (S1), so the one chat id this deployment is allowed to hear
// from is also the only chat it ever needs to send to.
type Telegram struct {
	BotToken        string
	WebhookSecret   string
	AllowedSenderID string
}

// Backup is parsed but not yet consumed. Litestream becomes the container
// entrypoint when the SQLite file exists to replicate.
type Backup struct {
	LitestreamReplicaURL string
}

// Load reads the environment and validates it. The error names the offending
// variable and why it was rejected, because that message is the only diagnostic
// a container that exits during startup leaves behind.
func Load() (Config, error) {
	var cfg Config
	var err error

	if cfg.Log.Level, err = envLogLevel("LOG_LEVEL", slog.LevelInfo); err != nil {
		return Config{}, err
	}

	if cfg.HTTP.Addr, err = envRequiredString("HTTP_ADDR", ":8000"); err != nil {
		return Config{}, err
	}

	if cfg.Data.Dir, err = envRequiredString("DATA_DIR", "/data"); err != nil {
		return Config{}, err
	}
	cfg.Data.DatabasePath = envString("DATABASE_PATH", "")

	if cfg.Files.ConfigDir, err = envRequiredString("CONFIG_DIR", "/config"); err != nil {
		return Config{}, err
	}

	if cfg.Schedule.DefaultTZ, err = envLocation("DEFAULT_TZ"); err != nil {
		return Config{}, err
	}

	// Required now: the scheduler resolves an adapter from this in the same
	// commit that reads it.
	if cfg.Transport.Notify, err = envRequiredString("NOTIFY_TRANSPORT", LoggingTransport); err != nil {
		return Config{}, err
	}
	cfg.Transport.Chat = envString("CHAT_TRANSPORT", "")

	// TELEGRAM_WEBHOOK_SECRET stays optional: nothing consumes it until P1's
	// webhook exists. BotToken and AllowedSenderID become required exactly
	// when NOTIFY_TRANSPORT names the adapter that reads them — required-when-
	// consumed applied to a value chosen by another value, not just by which
	// session has landed.
	cfg.Telegram.WebhookSecret = envString("TELEGRAM_WEBHOOK_SECRET", "")
	if cfg.Transport.Notify == TelegramTransport {
		if cfg.Telegram.BotToken, err = envRequiredString("TELEGRAM_BOT_TOKEN", ""); err != nil {
			return Config{}, err
		}
		if cfg.Telegram.AllowedSenderID, err = envRequiredString("ALLOWED_SENDER_ID", ""); err != nil {
			return Config{}, err
		}
	} else {
		cfg.Telegram.BotToken = envString("TELEGRAM_BOT_TOKEN", "")
		cfg.Telegram.AllowedSenderID = envString("ALLOWED_SENDER_ID", "")
	}

	cfg.Backup.LitestreamReplicaURL = envString("LITESTREAM_REPLICA_URL", "")

	return cfg, nil
}

// LogValue renders the configuration for the startup log line. Secrets are
// omitted entirely rather than redacted: a redaction that reports a length is
// still reporting something about a credential, and no operator has ever needed
// it.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTP.Addr),
		slog.String("log_level", c.Log.Level.String()),
		slog.String("data_dir", c.Data.Dir),
		slog.String("config_dir", c.Files.ConfigDir),
		slog.String("default_tz", c.Schedule.DefaultTZ.String()),

		// Which adapter is delivering reminders is the one setting whose wrong
		// value looks exactly like a working system, so it goes in the line that
		// gets read when someone asks why their phone is quiet.
		slog.String("notify_transport", c.Transport.Notify),
	)
}

func envString(key, def string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return def
}

func envRequiredString(key, def string) (string, error) {
	v, ok := lookup(key)
	if !ok {
		if def != "" {
			return def, nil
		}
		return "", missing(key)
	}
	if v == "" {
		return "", missing(key)
	}
	return v, nil
}

func envLocation(key string) (*time.Location, error) {
	v, ok := lookup(key)
	if !ok || v == "" {
		return nil, missing(key)
	}
	loc, err := time.LoadLocation(v)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", key, err)
	}
	return loc, nil
}

func envLogLevel(key string, def slog.Level) (slog.Level, error) {
	v, ok := lookup(key)
	if !ok || v == "" {
		return def, nil
	}
	switch strings.ToLower(v) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: %s: unknown level %q, want debug, info, warn or error", key, v)
	}
}

// lookup trims surrounding whitespace, because a value pasted into a .env file
// with a trailing space is a real failure mode and an unhelpful one to debug.
func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func missing(key string) error {
	return fmt.Errorf("config: %s is required", key)
}
