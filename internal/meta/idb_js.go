//go:build js && wasm

package meta

import (
	"database/sql"
	"fmt"
	"log/slog"

	_ "github.com/justwasm/sqlite3-vfs-idb"
)

func openIDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?vfs=idb", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open idb db: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init idb db: %w", err)
	}
	slog.Info("meta: using IndexedDB-backed database", "path", path)
	return db, nil
}
