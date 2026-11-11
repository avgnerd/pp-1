// Package db opens the SQLite database and applies schema migrations.
package db

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// migrations are applied in order; never edit an entry after release, append a new one.
var migrations = []string{
	`CREATE TABLE admin (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		username TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		is_default INTEGER NOT NULL DEFAULT 1,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL,
		ip TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX sessions_expires ON sessions(expires_at);
	CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE storages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		mount_path TEXT NOT NULL UNIQUE,
		root_path TEXT NOT NULL DEFAULT '/',
		app_key TEXT NOT NULL,
		app_secret_enc BLOB NOT NULL,
		refresh_token_enc BLOB,
		account_id TEXT NOT NULL DEFAULT '',
		account_email TEXT NOT NULL DEFAULT '',
		account_name TEXT NOT NULL DEFAULT '',
		cache_ttl_min INTEGER NOT NULL DEFAULT 30,
		status TEXT NOT NULL DEFAULT 'pending',
		auth_mode TEXT NOT NULL DEFAULT 'localhost',
		redirect_uri TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE TABLE oauth_states (
		state_hash TEXT PRIMARY KEY,
		storage_id INTEGER NOT NULL REFERENCES storages(id) ON DELETE CASCADE,
		redirect_uri TEXT NOT NULL DEFAULT '',
		expires_at INTEGER NOT NULL,
		used INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE media_probe (
		file_id TEXT NOT NULL,
		rev TEXT NOT NULL,
		data TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (file_id, rev)
	);`,
	// 2: millisecond timestamp of the last successful authorization (UI completion detection).
	`ALTER TABLE storages ADD COLUMN authorized_at INTEGER NOT NULL DEFAULT 0;`,
}

// Open opens (creating if needed) <dataDir>/app.db in WAL mode and migrates it.
func Open(dataDir string) (*sql.DB, error) {
	return OpenPath(filepath.Join(dataDir, "app.db"))
}

// OpenPath opens a database file (or ":memory:" style DSN) and migrates it.
func OpenPath(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a small pool avoids SQLITE_BUSY storms while still allowing parallel reads.
	d.SetMaxOpenConns(8)
	if err := d.Ping(); err != nil {
		d.Close()
		return nil, err
	}
	if err := Migrate(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Migrate applies pending migrations. It is idempotent.
func Migrate(d *sql.DB) error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := d.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&v)
	if err == sql.ErrNoRows {
		if _, err := d.Exec(`INSERT INTO schema_version(version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
