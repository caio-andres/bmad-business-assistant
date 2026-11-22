// Package store persists users, conversations, workspace documents, usage
// and audit in SQL. The same queries run on SQLite (default, zero setup) and
// Postgres (Supabase free tier, RDS/Aurora on AWS).
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver "pgx"
	_ "modernc.org/sqlite"             // sqlite driver "sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("não encontrado")

// ErrConflict signals a uniqueness or lease conflict.
var ErrConflict = errors.New("conflito")

// Store wraps the database.
type Store struct {
	db       *sql.DB
	postgres bool
	now      func() time.Time
}

// Open connects using a DATABASE_URL (postgres://...) or, when empty, a SQLite
// file under dataDir. It runs migrations.
func Open(ctx context.Context, databaseURL, dataDir string) (*Store, error) {
	var (
		db  *sql.DB
		err error
		pg  bool
	)
	switch {
	case strings.HasPrefix(databaseURL, "postgres://"), strings.HasPrefix(databaseURL, "postgresql://"):
		pg = true
		db, err = sql.Open("pgx", databaseURL)
		if err == nil {
			db.SetMaxOpenConns(10)
			db.SetConnMaxIdleTime(5 * time.Minute)
		}
	default:
		dsn := databaseURL
		if dsn == "" || dsn == "sqlite" {
			if dataDir == "" {
				dataDir = "data"
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return nil, err
			}
			dsn = filepath.ToSlash(filepath.Join(dataDir, "alicerce.db"))
		}
		dsn = strings.TrimPrefix(dsn, "sqlite://")
		if dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"
		db, err = sql.Open("sqlite", dsn)
		if err == nil {
			db.SetMaxOpenConns(4)
		}
	}
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: pg, now: time.Now}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("banco indisponível: %w", err)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Dialect returns "postgres" or "sqlite".
func (s *Store) Dialect() string {
	if s.postgres {
		return "postgres"
	}
	return "sqlite"
}

// Ping checks connectivity (health endpoint).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// SetClock overrides the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) q(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.q(query), args...)
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.q(query), args...)
}

func (s *Store) row(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.q(query), args...)
}

// --- time and ids ---------------------------------------------------------------

const tsLayout = "2006-01-02T15:04:05.000000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(v sql.NullString) time.Time {
	if !v.Valid || v.String == "" {
		return time.Time{}
	}
	t, err := time.Parse(tsLayout, v.String)
	if err != nil {
		t, _ = time.Parse(time.RFC3339Nano, v.String)
	}
	return t
}

func parseTSPtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTS(v)
	return &t
}

func (s *Store) stamp() string { return ts(s.now()) }

// NewID returns a random URL-safe identifier.
func NewID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique") || strings.Contains(m, "duplicate key")
}

// --- migrations -------------------------------------------------------------------

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		email TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT 'member',
		status TEXT NOT NULL DEFAULT 'active',
		password_hash TEXT,
		external_id TEXT,
		created_at TEXT NOT NULL,
		last_login_at TEXT
	);
	CREATE TABLE IF NOT EXISTS invites (
		token_hash TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT 'member',
		created_by TEXT,
		expires_at TEXT NOT NULL,
		used_at TEXT,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS sessions (
		id_hash TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL,
		user_agent TEXT
	);
	CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
	CREATE TABLE IF NOT EXISTS projects (
		id TEXT PRIMARY KEY,
		sigla TEXT NOT NULL,
		slug TEXT NOT NULL,
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		systems TEXT NOT NULL DEFAULT '[]',
		created_by TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE (sigla, slug)
	);
	CREATE TABLE IF NOT EXISTS conversations (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title TEXT NOT NULL DEFAULT '',
		project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
		state TEXT NOT NULL DEFAULT 'idle',
		active_skill TEXT NOT NULL DEFAULT '',
		active_agent TEXT NOT NULL DEFAULT '',
		transcript TEXT NOT NULL DEFAULT '[]',
		archived INTEGER NOT NULL DEFAULT 0,
		lease_owner TEXT,
		lease_until TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS conversations_user ON conversations(user_id, updated_at);
	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		options TEXT NOT NULL DEFAULT '[]',
		activities TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS messages_conv ON messages(conversation_id, created_at);
	CREATE TABLE IF NOT EXISTS workspace_files (
		conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		path TEXT NOT NULL,
		content {{BLOB}} NOT NULL,
		size INTEGER NOT NULL,
		updated_by TEXT NOT NULL DEFAULT 'assistant',
		updated_at TEXT NOT NULL,
		PRIMARY KEY (conversation_id, path)
	);
	CREATE TABLE IF NOT EXISTS workspace_versions (
		id TEXT PRIMARY KEY,
		conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		path TEXT NOT NULL,
		content {{BLOB}} NOT NULL,
		updated_by TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS versions_file ON workspace_versions(conversation_id, path, created_at);
	CREATE TABLE IF NOT EXISTS publications (
		id TEXT PRIMARY KEY,
		conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL,
		project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
		branch TEXT NOT NULL,
		pr_url TEXT NOT NULL DEFAULT '',
		commit_sha TEXT NOT NULL DEFAULT '',
		files TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL,
		created_by TEXT,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS publications_project ON publications(project_id, created_at);
	CREATE TABLE IF NOT EXISTS usage_records (
		id TEXT PRIMARY KEY,
		user_id TEXT,
		conversation_id TEXT,
		purpose TEXT NOT NULL,
		model TEXT NOT NULL,
		input_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		cost_usd REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS usage_user ON usage_records(user_id, created_at);
	CREATE TABLE IF NOT EXISTS audit_log (
		id TEXT PRIMARY KEY,
		user_id TEXT,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		ip TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS audit_time ON audit_log(created_at);
	CREATE TABLE IF NOT EXISTS repo_summaries (
		system TEXT PRIMARY KEY,
		summary TEXT NOT NULL,
		source_ref TEXT NOT NULL DEFAULT '',
		generated_at TEXT NOT NULL
	)`,
	// 2: blob SHAs of files loaded from / published to the central workspace
	`CREATE TABLE IF NOT EXISTS workspace_bases (
		conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
		path TEXT NOT NULL,
		sha TEXT NOT NULL,
		PRIMARY KEY (conversation_id, path)
	)`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	blob := "BLOB"
	if s.postgres {
		blob = "BYTEA"
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, stmt := range strings.Split(strings.ReplaceAll(migrations[i], "{{BLOB}}", blob), ";\n") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), i+1, s.stamp()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
