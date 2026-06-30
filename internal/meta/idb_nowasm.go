//go:build !js || !wasm

package meta

import (
	"database/sql"
	"fmt"
)

func openIDB(_ string) (*sql.DB, error) {
	return nil, fmt.Errorf("idb vfs not available on this platform")
}
