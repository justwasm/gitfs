// Package main demonstrates using the gitfs package to clone a repository
// and read its contents through Go's standard io/fs interface.
//
// It clones https://github.com/justwasm/gitfs (blobless), builds a snapshot,
// and reads files without mounting FUSE.
//
// Usage:
//
//	go run ./examples/gitfs
package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/gitfs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func main() {
	ctx := context.Background()

	// ── 1. Create temp state directory ───────────────────────────────────

	stateRoot, err := os.MkdirTemp("", "gitfs-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(stateRoot)
	fmt.Println("state root:", stateRoot)

	// ── 2. Configure the repository ──────────────────────────────────────

	cfg := model.RepoConfig{
		ID:              "example",
		Name:            "example",
		RemoteURL:       "https://github.com/justwasm/gitfs",
		Branch:          "main",
		MountRoot:       filepath.Join(stateRoot, "mnt"),
		MountPath:       filepath.Join(stateRoot, "mnt", "example"),
		GitDir:          filepath.Join(stateRoot, "repos", "example", "git"),
		OverlayDir:      filepath.Join(stateRoot, "overlays", "example"),
		BlobCacheDir:    filepath.Join(stateRoot, "cache", "blobs", "example"),
		MetaDBPath:      filepath.Join(stateRoot, "meta", "example.sqlite"),
		OverlayDBPath:   filepath.Join(stateRoot, "overlays", "example", "meta.sqlite"),
	}

	// ── 3. Clone the repository (blobless) ───────────────────────────────

	gitStore := gitstore.New(nil)
	defer gitStore.Close()

	fmt.Println("cloning", cfg.RemoteURL, "...")
	if err := gitStore.CloneBlobless(ctx, cfg); err != nil {
		log.Fatalf("clone: %v", err)
	}

	headOID, headRef, err := gitStore.ResolveHEAD(ctx, cfg)
	if err != nil {
		log.Fatalf("resolve HEAD: %v", err)
	}
	fmt.Printf("HEAD: %s (%s)\n", headOID[:12], headRef)

	// ── 4. Build the snapshot ────────────────────────────────────────────

	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		log.Fatalf("snapshot store: %v", err)
	}
	defer snap.Close()

	nodes, err := gitStore.BuildTreeIndex(ctx, cfg, headOID)
	if err != nil {
		log.Fatalf("build tree index: %v", err)
	}
	gen, err := snap.PublishGeneration(ctx, headOID, headRef, nodes)
	if err != nil {
		log.Fatalf("publish snapshot: %v", err)
	}
	fmt.Printf("snapshot: generation=%d, %d nodes\n", gen, len(nodes))

	// ── 5. Create overlay store ──────────────────────────────────────────

	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		log.Fatalf("overlay store: %v", err)
	}
	defer ov.Close()

	// ── 6. Create hydrator ───────────────────────────────────────────────

	h := hydrator.New(gitStore)
	h.Start(2, cfg)
	defer h.Stop()

	// ── 7. Build resolver + engine + fs.FS ───────────────────────────────

	resolver := gitfs.NewResolver(snap, ov)
	resolver.SetGeneration(gen)
	resolver.SetCommitTime(time.Now().Unix())

	engine := gitfs.NewEngine(resolver, cfg, ov, h)

	fsys := gitfs.New(engine, resolver)

	// ── 8. Read files ────────────────────────────────────────────────────

	fmt.Println("\n--- Root directory ---")
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		log.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		fmt.Printf("  %s [%s]\n", e.Name(), kind)
	}

	fmt.Println("\n--- Walk ---")
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fmt.Printf("  %s\n", path)
		return nil
	})

	// Try reading a file (any text file in the repo).
	fmt.Println("\n--- Read files ---")
	for _, name := range []string{"README.md", "go.mod", "LICENSE"} {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			fmt.Printf("  %s: %v\n", name, err)
			continue
		}
		// Print first 200 bytes.
	preview := data
		if len(preview) > 200 {
			preview = preview[:200]
		}
		fmt.Printf("  %s (%d bytes):\n%s\n\n", name, len(data), preview)
	}

	// ── 9. Stat a file ───────────────────────────────────────────────────

	fmt.Println("--- Stat ---")
	for _, name := range []string{".", "go.mod"} {
		fi, err := fs.Stat(fsys, name)
		if err != nil {
			fmt.Printf("  %s: %v\n", name, err)
			continue
		}
		fmt.Printf("  %s: size=%d mode=%s isDir=%v\n", name, fi.Size(), fi.Mode(), fi.IsDir())
	}

	// ── 10. Use standard library functions ───────────────────────────────

	fmt.Println("\n--- fs.Glob ---")
	matches, err := fs.Glob(fsys, "**/*.go")
	if err != nil {
		// fs.Glob doesn't support **, use Walk instead.
		fmt.Println("  (fs.Glob doesn't support **, skipping)")
	}
	for _, m := range matches {
		fmt.Printf("  %s\n", m)
	}

	// Walk to find .go files.
	fmt.Println("\n--- .go files ---")
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Ext(path) == ".go" {
			fmt.Printf("  %s\n", path)
		}
		return nil
	})

	_ = io.Discard // ensure io is used
}
