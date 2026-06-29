//go:build js || wasm

package main

import (
	"context"
	"fmt"
	"io/fs"
)

func cloneAndBuildFSImpl(_ context.Context) fs.FS {
	fmt.Println("ERROR: --repo requires git and SQLite (not available in WASM)")
	fmt.Println("       Use the in-memory demo without --repo, or run on a native platform.")
	return nil
}
