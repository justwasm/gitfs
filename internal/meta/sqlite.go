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

	// Tier 1: native file-based SQLite.
	if path != ":memory:" {
		if db, err := openFile(path); err == nil {
			slog.Info("meta: using file-based database", "path", path)
			return db, nil
		} else {
			slog.Warn("meta: file-based db failed, trying IDB VFS", "path", path, "err", err)
		}
	}

	// Tier 2: IDB VFS (IndexedDB on WASM, in-memory on native).
	if path != ":memory:" {
		if db, err := openIDB(path); err == nil {
			slog.Info("meta: using IDB VFS database", "path", path)
			return db, nil
		} else {
			slog.Warn("meta: IDB VFS failed, falling back to memory", "path", path, "err", err)
		}
	}

	// Tier 3: pure in-memory.
	slog.Info("meta: using in-memory database")
	return openMemory()
}

func openFile(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open db file: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db file: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init db file: %w", err)
	}
	return db, nil
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
