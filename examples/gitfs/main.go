// Package main demonstrates using the gitfs package to clone a repository
// and read its contents through Go's standard io/fs interface.
//
// Usage:
//
//	go run ./examples/gitfs
//	go run ./examples/gitfs --repo https://github.com/cloudflare/artifact-fs
//	go run ./examples/gitfs --repo https://github.com/golang/go --branch master
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/gitfs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

var (
	repoURL = flag.String("repo", "https://github.com/justwasm/gitfs", "git remote URL to clone")
	branch  = flag.String("branch", "main", "branch to check out")
)

func main() {
	flag.Parse()
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
		ID:            "example",
		Name:          "example",
		RemoteURL:     *repoURL,
		Branch:        *branch,
		MountRoot:     filepath.Join(stateRoot, "mnt"),
		MountPath:     filepath.Join(stateRoot, "mnt", "example"),
		GitDir:        filepath.Join(stateRoot, "repos", "example", "git"),
		OverlayDir:    filepath.Join(stateRoot, "overlays", "example"),
		BlobCacheDir:  filepath.Join(stateRoot, "cache", "blobs", "example"),
		MetaDBPath:    filepath.Join(stateRoot, "meta", "example.sqlite"),
		OverlayDBPath: filepath.Join(stateRoot, "overlays", "example", "meta.sqlite"),
	}

	// ── 3. Clone the repository (blobless) ───────────────────────────────

	gitStore := gitstore.New(nil)
	defer gitStore.Close()

	fmt.Println("cloning", cfg.RemoteURL, "("+cfg.Branch+")", "...")
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

	// ── 8. Read the root directory ───────────────────────────────────────

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

	// ── 9. Walk the entire tree ──────────────────────────────────────────

	fmt.Println("\n--- Walk ---")
	var fileCount, dirCount int
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirCount++
		} else {
			fileCount++
		}
		return nil
	})
	fmt.Printf("  %d directories, %d files\n", dirCount, fileCount)

	// ── 10. Discover and read a few files dynamically ────────────────────

	fmt.Println("\n--- Sample files ---")
	var samples []string
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || len(samples) >= 3 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".md" || ext == ".go" || ext == ".txt" || ext == ".mod" {
			samples = append(samples, path)
		}
		return nil
	})
	if len(samples) == 0 {
		// Fallback: just grab the first 3 files.
		fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || len(samples) >= 3 {
				return nil
			}
			samples = append(samples, path)
			return nil
		})
	}
	for _, name := range samples {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			fmt.Printf("  %s: %v\n", name, err)
			continue
		}
		preview := data
		if len(preview) > 200 {
			preview = preview[:200]
		}
		fmt.Printf("  %s (%d bytes):\n    %s\n\n", name, len(data), strings.ReplaceAll(string(preview), "\n", "\n    "))
	}

	// ── 11. Stat the root and a file ─────────────────────────────────────

	fmt.Println("--- Stat ---")
	fi, err := fs.Stat(fsys, ".")
	if err != nil {
		log.Fatalf("stat root: %v", err)
	}
	fmt.Printf("  .: size=%d mode=%s isDir=%v\n", fi.Size(), fi.Mode(), fi.IsDir())

	if len(samples) > 0 {
		fi, err = fs.Stat(fsys, samples[0])
		if err != nil {
			fmt.Printf("  %s: %v\n", samples[0], err)
		} else {
			fmt.Printf("  %s: size=%d mode=%s isDir=%v\n", samples[0], fi.Size(), fi.Mode(), fi.IsDir())
		}
	}
}
