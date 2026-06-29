package meta

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func OpenDB(path string) (*sql.DB, error) {
	if path == "" {
		path = ":memory:"
	}

	// Try file-based DB first.
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			slog.Warn("meta: cannot create db dir, falling back to memory", "path", path, "err", err)
			return openMemory()
		}
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			slog.Warn("meta: cannot open db file, falling back to memory", "path", path, "err", err)
			return openMemory()
		}
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
			db.Close()
			slog.Warn("meta: cannot init db file, falling back to memory", "path", path, "err", err)
			return openMemory()
		}
		return db, nil
	}

	return openMemory()
}

func openMemory() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init memory db: %w", err)
	}
	slog.Info("meta: using in-memory database")
	return db, nil
}

func ExecMigrations(ctx context.Context, db *sql.DB, stmts []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
