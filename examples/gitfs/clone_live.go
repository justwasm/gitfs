//go:build !js && !wasm

package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func cloneAndBuildFSImpl(ctx context.Context) fs.FS {
	stateRoot, err := filepath.Abs(".gitfs-example-state")
	if err != nil {
		log.Fatal(err)
	}

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

	gitStore := gitstore.New(nil)
	// Note: no defer gitStore.Close() — it must stay open for hydrator blob reads.

	fmt.Println("cloning ...")
	if err := gitStore.CloneBlobless(ctx, cfg); err != nil {
		log.Fatalf("clone: %v", err)
	}

	headOID, headRef, err := gitStore.ResolveHEAD(ctx, cfg)
	if err != nil {
		log.Fatalf("resolve HEAD: %v", err)
	}
	fmt.Printf("HEAD: %s (%s)\n", headOID[:12], headRef)

	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		log.Fatalf("snapshot: %v", err)
	}

	nodes, err := gitStore.BuildTreeIndex(ctx, cfg, headOID)
	if err != nil {
		log.Fatalf("tree index: %v", err)
	}
	gen, err := snap.PublishGeneration(ctx, headOID, headRef, nodes)
	if err != nil {
		log.Fatalf("publish: %v", err)
	}
	fmt.Printf("snapshot: gen=%d, %d nodes\n", gen, len(nodes))

	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		log.Fatalf("overlay: %v", err)
	}

	h := hydrator.New(gitStore)
	h.Start(2, cfg)

	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	resolver.SetCommitTime(time.Now().Unix())

	engine := &fusefs.Engine{
		Resolver: resolver,
		Repo:     cfg,
		Overlay:  ov,
		Hydrator: h,
	}

	return fusefs.NewArtifactFS(engine, resolver)
}
