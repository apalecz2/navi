package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aidenpaleczny/navi/internal/domain"
	"github.com/aidenpaleczny/navi/internal/store/sqlc"
)

// migrationFS carries the schema into the binary. Embedding is what keeps D11
// intact: one static binary with no migration step to forget on deploy and no
// external tool to install on the host.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// SchemaVersionKey is the kv key holding the highest applied migration. There
// is no schema_migrations table because at this scale the framework is more
// machinery than the problem justifies, and a single-user database can afford a
// restore from R2 if a migration goes wrong.
const SchemaVersionKey = "schema_version"

// migrationName matches NNNN_lower_snake_case.sql. Anything else in the
// directory is a mistake worth failing on rather than skipping quietly.
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version int
	name    string
	body    string
}

// migrate applies every migration newer than the recorded version, each inside
// one transaction that also records the new version. SQLite DDL is
// transactional, so a migration that fails partway leaves both the schema and
// the version untouched.
func migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	all, err := loadMigrations()
	if err != nil {
		return err
	}

	current, err := currentVersion(ctx, db)
	if err != nil {
		return err
	}

	applied := 0
	for _, m := range all {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", m.name, err)
		}
		log.Info("migration applied", "version", m.version, "name", m.name)
		applied++
	}

	if applied == 0 {
		log.Debug("schema up to date", "version", current)
	}
	return nil
}

// applyMigration runs one file and records its version in the same transaction.
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, stmt := range splitStatements(m.body) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement %d: %w", i+1, err)
		}
	}

	// The first migration creates kv, so by the time this runs the table
	// exists — which is why the version can live in kv rather than needing a
	// table of its own bootstrapped ahead of the schema.
	if err := sqlc.New(tx).SetKV(ctx, sqlc.SetKVParams{
		Key:       SchemaVersionKey,
		Value:     strconv.Itoa(m.version),
		UpdatedAt: domain.FormatTime(time.Now()),
	}); err != nil {
		return fmt.Errorf("record version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// currentVersion reads the applied version, treating a database with no kv
// table as version zero. A fresh volume is the normal first run, not an error,
// and this is the only query in the package that has to cope with the schema
// not existing yet.
func currentVersion(ctx context.Context, db *sql.DB) (int, error) {
	var tables int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'kv'`,
	).Scan(&tables)
	if err != nil {
		return 0, fmt.Errorf("store: read schema state: %w", err)
	}
	if tables == 0 {
		return 0, nil
	}

	value, err := sqlc.New(db).GetKV(ctx, SchemaVersionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}

	version, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("store: schema version %q is not a number: %w", value, err)
	}
	return version, nil
}

// loadMigrations reads and orders the embedded files, rejecting a numbering
// mistake at startup. Two files claiming the same version, or a name the
// convention does not cover, is the kind of error that otherwise shows up as a
// migration that never ran.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationName.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("store: migration %q does not match NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration %q: %w", e.Name(), err)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", other, e.Name(), version)
		}
		if version == 0 {
			return nil, fmt.Errorf("store: migration %q uses version 0, which means unmigrated", e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: e.Name(), body: string(body)})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	return migrations, nil
}

// splitStatements breaks a migration file into individual statements on a
// semicolon at end of line.
//
// This is deliberately not a SQL parser and does not need to be: it runs only
// over the files in this package's migrations directory, which contain no
// triggers and no semicolons inside string literals. Splitting here rather than
// handing the whole file to the driver keeps a driver behaviour out of the
// startup path and puts the failing statement's number in the error.
func splitStatements(body string) []string {
	var (
		statements []string
		current    strings.Builder
	)

	flush := func() {
		stmt := current.String()
		current.Reset()
		if hasSQL(stmt) {
			statements = append(statements, strings.TrimSpace(stmt))
		}
	}

	for _, line := range strings.Split(body, "\n") {
		current.WriteString(line)
		current.WriteString("\n")
		if strings.HasSuffix(strings.TrimRight(line, " \t\r"), ";") {
			flush()
		}
	}
	flush()

	return statements
}

// hasSQL reports whether a chunk contains anything but blank lines and
// full-line comments, so the comment block trailing the last statement is not
// executed as an empty one.
func hasSQL(chunk string) bool {
	for _, line := range strings.Split(chunk, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		return true
	}
	return false
}
