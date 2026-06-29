//go:build js || wasm

package main

import (
	"context"
	"io/fs"
	"log"
)

func cloneAndBuildFSImpl(_ context.Context) fs.FS {
	log.Fatal("ERROR: --repo requires git and a native OS (not available in WASM).\n" +
		"       Run without --repo for the in-memory demo, or use a native platform.")
	return nil // unreachable
}
