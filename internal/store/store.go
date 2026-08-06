// Package store is the repository module: every SQL statement this service
// runs lives here or in the .sql files this package embeds and compiles.
//
// Loop bodies, endpoint handlers, and agent tools call named methods that
// return domain types. That is ordinary structure, and it is also the only
// hedge the design makes against a scope assumption it has not committed to
// (Q-13): a cross-cutting schema change is a bounded edit inside one package
// and an unreviewable audit outside it, because the expensive part of
// retrofitting a column is finding every query, and a miss is silent and looks
// like working code.
//
// Concurrency is one writer and concurrent readers, which is what SQLite in WAL
// mode is good at. That shape is enforced by the two pools below rather than by
// discipline.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	// The pure-Go SQLite driver. mattn/go-sqlite3 is the more common one and
	// needs cgo, which means a C toolchain in the build and a dynamically
	// linked binary; D11 wants a static binary in a scratch image, so the
	// driver has to be this one. It is slower under heavy concurrent write
	// load, which is a benchmark this workload never runs (D-002, D-021).
	_ "modernc.org/sqlite"

	"github.com/aidenpaleczny/navi/internal/config"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// ErrNotFound is returned when a row does not exist. Callers test it with
// errors.Is rather than comparing against sql.ErrNoRows, which is the driver's
// business and not theirs.
var ErrNotFound = errors.New("store: not found")

// OverdueGrace is how far past its start time a pending occurrence must be
// before it counts as overdue.
//
// It lives here so /healthz and the navi_pending_overdue metric cannot drift
// apart and neither caller computes a threshold of its own. Two scheduler
// intervals of slack is what keeps a healthy system from flapping between zero
// and one every 30 seconds: an occurrence due right now is not late, it is
// waiting for the next tick.
const OverdueGrace = 60 * time.Second

// pragmas are applied on open, as DSN parameters rather than as an Exec after
// the fact. Pragmas are per-connection and database/sql opens connections
// lazily, so running them once after sql.Open sets them on the first connection
// and silently misses every connection opened later — including, on a
// foreign_keys miss, every cascade the schema depends on.
const pragmas = "_journal_mode=WAL" +
	"&_busy_timeout=5000" +
	"&_foreign_keys=1" +
	"&_synchronous=NORMAL"

// txlock makes every transaction on the writer handle a BEGIN IMMEDIATE, which
// takes the write lock up front instead of risking a mid-transaction upgrade
// failure. The scheduler's claim needs exactly this and database/sql offers no
// direct way to ask for it; a DSN parameter is the whole mechanism, and it
// leaves BeginTx as ordinary database/sql with its rollback-on-cancel and
// bad-connection handling intact.
//
// The hand-rolled alternative — sql.Conn plus ExecContext("BEGIN IMMEDIATE") —
// can return a connection to the pool still inside a transaction, and with
// SetMaxOpenConns(1) that is a wedged process rather than a slow query.
const txlock = "&_txlock=immediate"

// readerConns bounds the read pool. Readers do not block each other in WAL
// mode; this is a ceiling on file handles, not on concurrency, and four is more
// than a single-user service with five loops and one HTTP listener will use.
const readerConns = 4

// Store holds the two pools and the queries bound to the read pool.
type Store struct {
	writer *sql.DB
	reader *sql.DB

	// read is bound to the reader pool. Writes go through tx, which binds its
	// own Queries to the transaction, so there is no handle on this struct that
	// can write outside one.
	read *sqlc.Queries

	log *slog.Logger
}

// Open prepares the data directory, opens both pools, and applies any pending
// migrations. The writer is opened and migrated before the reader exists, so a
// fresh database is fully created by the one connection allowed to create it.
func Open(ctx context.Context, cfg config.Data, log *slog.Logger) (*Store, error) {
	path := cfg.DBPath()

	// 0700 rather than 0755: the directory holds the database and, from P6, a
	// generated secret (D10).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create data directory %s: %w", filepath.Dir(path), err)
	}

	writer, err := open(ctx, path+"?"+pragmas+txlock)
	if err != nil {
		return nil, fmt.Errorf("store: open writer %s: %w", path, err)
	}

	// One connection is what makes "single writer" true. Without it
	// database/sql opens several and lets two writers collide, which SQLite
	// reports as a busy database rather than as the bug it is.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)

	// Never recycled: reopening costs the WAL handshake and the pragmas for no
	// benefit on a connection that lives as long as the process.
	writer.SetConnMaxLifetime(0)

	s := &Store{writer: writer, log: log}

	if err := migrate(ctx, writer, log); err != nil {
		_ = writer.Close()
		return nil, err
	}

	reader, err := open(ctx, path+"?"+pragmas)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("store: open reader %s: %w", path, err)
	}
	reader.SetMaxOpenConns(readerConns)
	reader.SetMaxIdleConns(readerConns)
	reader.SetConnMaxLifetime(0)

	s.reader = reader
	s.read = sqlc.New(reader)
	return s, nil
}

// open dials a pool and proves it works. sql.Open is lazy, so without the ping
// a bad path or an unwritable directory surfaces at the first query instead of
// at startup, which is the difference between a container that fails to start
// and one that fails to fire a reminder.
func open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Close shuts both pools down.
func (s *Store) Close() error {
	var errs []error
	if s.reader != nil {
		if err := s.reader.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close reader: %w", err))
		}
	}
	if s.writer != nil {
		if err := s.writer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("store: close writer: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Ping reports whether the database is reachable. Both pools are checked: a
// reader that works and a writer that does not is a disk-full or read-only
// filesystem, which looks healthy from every query /healthz would otherwise
// run.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.reader.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping reader: %w", err)
	}
	if err := s.writer.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping writer: %w", err)
	}
	return nil
}

// tx runs fn inside one BEGIN IMMEDIATE transaction on the writer. It is the
// only place BeginTx appears, so no caller can start a transaction that is not
// immediate and none can hold one open across a return.
//
// The deferred Rollback is a no-op after a successful Commit, and is what
// releases the single writer connection when fn returns an error or panics.
func (s *Store) tx(ctx context.Context, fn func(*sqlc.Queries) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(sqlc.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// notFound maps the driver's no-rows error onto this package's sentinel and
// leaves every other error alone.
func notFound(op string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, err)
}
