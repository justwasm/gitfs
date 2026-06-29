// Package gitfs provides an fs.FS adapter for artifact-fs repositories.
//
// Artifact-fs is a git-backed FUSE filesystem. This package exposes its core
// read/write engine as a standard Go [io/fs.FS], allowing in-process use
// without mounting FUSE.
//
// # Getting started
//
// Construct an [Engine] and [Resolver] from your snapshot and overlay stores,
// then call [New] or [NewWritable] to get an fs.FS:
//
//	fsys := gitfs.New(engine, resolver)
//	data, err := fs.ReadFile(fsys, "README.md")
//
// # Interfaces
//
// [New] returns a read-only [io/fs.FS] that also satisfies [io/fs.ReadDirFS]
// and [io/fs.StatFS]. [NewWritable] returns a [WritableFS] that adds write
// operations (WriteFile, Mkdir, Remove, Rename) on top.
//
// # Symlink semantics
//
// io/fs has no symlink support. Opening a symlink resolves its target and
// returns the target's content. [fs.Stat] still reports [fs.ModeSymlink] on
// the link itself. Base-only symlinks (not in the overlay) are not yet
// supported and return [fs.ErrInvalid].
//
// # Path conventions
//
// All paths follow io/fs conventions: relative, no leading slash, no trailing
// slash, no "..". The root is named ".". Use [fs.ValidPath] to check before
// calling any method.
package gitfs

import (
	"context"
	"io/fs"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Engine is the core read/write engine for artifact-fs.
type Engine = fusefs.Engine

// Resolver merges snapshot and overlay views of the repository tree.
type Resolver = fusefs.Resolver

// WritableFS extends [io/fs.FS] with write operations backed by the overlay.
type WritableFS interface {
	fs.FS
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	Mkdir(ctx context.Context, name string, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName string) error
}

// New creates a read-only [io/fs.FS] backed by the given engine and resolver.
//
// The returned value also satisfies [io/fs.ReadDirFS] and [io/fs.StatFS].
func New(engine *Engine, resolver *Resolver) fs.FS {
	return fusefs.NewArtifactFS(engine, resolver)
}

// NewWritable creates a [WritableFS] backed by the given engine and resolver.
//
// Write operations delegate to the engine's overlay layer.
func NewWritable(engine *Engine, resolver *Resolver) WritableFS {
	return fusefs.NewWritableFS(engine, resolver)
}

// WithContext returns a shallow copy of fsys that uses ctx for all I/O.
//
// This is useful when the caller has a request-scoped context (e.g. from an
// HTTP handler) and wants cancellation to propagate into filesystem reads.
func WithContext(fsys fs.FS, ctx context.Context) fs.FS {
	type contextFS interface {
		WithContext(context.Context) fs.FS
	}
	if cfs, ok := fsys.(contextFS); ok {
		return cfs.WithContext(ctx)
	}
	return fsys
}

// NewResolver creates a new [Resolver] with the given snapshot and overlay stores.
func NewResolver(snapshot model.SnapshotStore, overlay model.OverlayStore) *Resolver {
	return &fusefs.Resolver{Snapshot: snapshot, Overlay: overlay}
}

// NewEngine creates a new [Engine] with the given dependencies.
func NewEngine(resolver *Resolver, repo model.RepoConfig, overlay model.OverlayStore, hydrator model.Hydrator) *Engine {
	return &fusefs.Engine{
		Resolver: resolver,
		Repo:     repo,
		Overlay:  overlay,
		Hydrator: hydrator,
	}
}
