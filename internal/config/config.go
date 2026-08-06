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
	"strings"
	"time"
)

// Config is the whole of the process configuration, grouped by the component
// that consumes it. Components take their own group, never the whole struct.
type Config struct {
	HTTP     HTTP
	Log      Log
	Data     Data
	Schedule Schedule
	Telegram Telegram
	Backup   Backup
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

	// DatabasePath is parsed now and consumed when the store lands. Empty means
	// "DATABASE_PATH was not set"; the store session decides whether to default
	// it under Dir or to require it.
	DatabasePath string
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

// Telegram is parsed but not yet consumed. The bot token and the webhook secret
// are borrowed credentials and come from the environment with no default (D9).
type Telegram struct {
	BotToken        string
	WebhookSecret   string
	AllowedSenderID string

	// NotifyTransport and ChatTransport name the adapter filling each transport
	// role. Both point at Telegram to start with (D-006); they stay separate
	// because that split is the seam a dedicated push channel drops into later.
	NotifyTransport string
	ChatTransport   string
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

	if cfg.Schedule.DefaultTZ, err = envLocation("DEFAULT_TZ"); err != nil {
		return Config{}, err
	}

	cfg.Telegram = Telegram{
		BotToken:        envString("TELEGRAM_BOT_TOKEN", ""),
		WebhookSecret:   envString("TELEGRAM_WEBHOOK_SECRET", ""),
		AllowedSenderID: envString("ALLOWED_SENDER_ID", ""),
		NotifyTransport: envString("NOTIFY_TRANSPORT", ""),
		ChatTransport:   envString("CHAT_TRANSPORT", ""),
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
		slog.String("default_tz", c.Schedule.DefaultTZ.String()),
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
