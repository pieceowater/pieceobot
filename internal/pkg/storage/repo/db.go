// Package repo is the sqlite-backed persistence layer: schema + one small
// repo type per table, mirroring lotof.tg.capital.bot's state/repo package
// (except this bot's state lives in a local sqlite file instead of
// capital.gtw's key-value store -- there's no such backend here).
package repo

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id INTEGER NOT NULL,
    from_me INTEGER NOT NULL,
    text TEXT NOT NULL,
    ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_chat_ts ON messages(chat_id, ts);
CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages(ts);

CREATE TABLE IF NOT EXISTS llm_calls (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL,
    result TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_llm_calls_ts ON llm_calls(ts);
CREATE INDEX IF NOT EXISTS idx_llm_calls_chat_ts ON llm_calls(chat_id, ts);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_state (
    chat_id INTEGER PRIMARY KEY,
    muted INTEGER NOT NULL DEFAULT 0,
    last_owner_msg_ts INTEGER,
    last_message_from_me INTEGER NOT NULL DEFAULT 0
);
-- display_name added later via the migration below -- CREATE TABLE IF NOT
-- EXISTS doesn't alter an already-existing table.

-- One row per active Telegram Business connection, refreshed on every
-- business_connection update. is_owner gates everything in telegram/svc --
-- see Service.handleBusinessConnection's comment for why.
CREATE TABLE IF NOT EXISTS business_connections (
    id TEXT PRIMARY KEY,
    owner_user_id INTEGER NOT NULL,
    is_owner INTEGER NOT NULL,
    is_enabled INTEGER NOT NULL,
    can_reply INTEGER NOT NULL,
    updated_ts INTEGER NOT NULL
);
`

func Connect(dbPath string) (*sql.DB, error) {
	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("repo: create data dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("repo: open: %w", err)
	}
	// A single connection avoids "database is locked" errors from concurrent
	// goroutines -- SQLite allows only one writer at a time anyway, and the
	// bot's write volume never justifies a real pool.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("repo: set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("repo: set busy_timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("repo: apply schema: %w", err)
	}
	if err := addColumnIfMissing(db, "chat_state", "display_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return nil, fmt.Errorf("repo: migrate chat_state.display_name: %w", err)
	}
	if err := addColumnIfMissing(db, "chat_state", "rate_limit_notice_ts", "INTEGER"); err != nil {
		return nil, fmt.Errorf("repo: migrate chat_state.rate_limit_notice_ts: %w", err)
	}
	if err := addColumnIfMissing(db, "messages", "is_manual", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, fmt.Errorf("repo: migrate messages.is_manual: %w", err)
	}
	// Index added after the column migration -- CREATE INDEX at schema-init
	// time would fail on a pre-migration table that doesn't have the column yet.
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_messages_manual ON messages(is_manual, ts)"); err != nil {
		return nil, fmt.Errorf("repo: create idx_messages_manual: %w", err)
	}
	return db, nil
}

// addColumnIfMissing runs an idempotent ALTER TABLE ... ADD COLUMN -- the
// lightweight migration path for a schema that only ever grows columns.
// SQLite has no "ADD COLUMN IF NOT EXISTS", so a duplicate-column error is
// the expected, ignored outcome on every run after the first.
func addColumnIfMissing(db *sql.DB, table, column, def string) error {
	_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}
