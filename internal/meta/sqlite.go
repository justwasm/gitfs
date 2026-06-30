package meta

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/justwasm/sqlite3-vfs-idb"
)

var pragmas = map[string]string{
	"journal_mode": "WAL",
	"foreign_keys": "ON",
	"busy_timeout": "5000",
}

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
	dsn := fmt.Sprintf("file:%s?_txlock=immediate", path)
	db, err := driver.Open(dsn, pragmaSetter)
	if err != nil {
		return nil, fmt.Errorf("open db file: %w", err)
	}
	slog.Info("meta: using file-based database", "path", path)
	return db, nil
}

func openIDB(path string) (*sql.DB, error) {
	if runtime.GOOS != "js" {
		return nil, fmt.Errorf("idb vfs not available on this platform")
	}
	dsn := fmt.Sprintf("file:%s?_txlock=immediate&vfs=idb", path)
	db, err := driver.Open(dsn, pragmaSetter)
	if err != nil {
		return nil, fmt.Errorf("open idb db: %w", err)
	}
	slog.Info("meta: using IndexedDB-backed database", "path", path)
	return db, nil
}

func openMemory() (*sql.DB, error) {
	dsn := "file::memory:?_txlock=immediate"
	db, err := driver.Open(dsn, pragmaSetter)
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}
	slog.Info("meta: using in-memory database")
	return db, nil
}

func pragmaSetter(c *sqlite3.Conn) error {
	for name, value := range pragmas {
		if err := c.Exec(fmt.Sprintf("PRAGMA %s = %s;", name, value)); err != nil {
			return fmt.Errorf("set pragma %q: %w", name, err)
		}
	}
	return nil
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
