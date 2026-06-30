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

// OpenDB opens a SQLite database with a 3-tier fallback:
//  1. Native file-based SQLite (best for WASM-less environments)
//  2. IDB VFS (IndexedDB persistence on WASM, in-memory on native)
//  3. Pure in-memory (last resort, no persistence)
func OpenDB(path string) (*sql.DB, error) {
	if path == "" {
		path = ":memory:"
	}

	// Tier 1: native file-based SQLite.
	if path != ":memory:" {
		if db, err := openFile(path); err == nil {
			return db, nil
		}
	}

	// Tier 2: IDB VFS (IndexedDB on WASM, in-memory on native).
	if path != ":memory:" {
		if db, err := openIDB(path); err == nil {
			return db, nil
		}
	}

	// Tier 3: pure in-memory.
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
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init db file: %w", err)
	}
	slog.Info("meta: using file-based database", "path", path)
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
