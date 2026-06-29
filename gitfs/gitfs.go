// Package gitfs provides an fs.FS adapter for artifact-fs repositories.
//
// This package depends only on the portable fsadapter core and has no
// dependency on FUSE or SQLite. Concrete store implementations (snapshot,
// overlay) can be swapped; for example, an in-memory store for WASM or
// a pure-Go SQLite driver for environments without libc.
//
// # Getting started
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

	"github.com/cloudflare/artifact-fs/internal/fsadapter"
)

// ReadEngine provides read access to file content.
type ReadEngine = fsadapter.ReadEngine

// WriteEngine extends ReadEngine with write operations.
type WriteEngine = fsadapter.WriteEngine

// Resolver resolves paths and provides directory/attribute information.
type Resolver = fsadapter.Resolver

// WritableFS extends [io/fs.FS] with write operations backed by the overlay.
type WritableFS = fsadapter.WritableFS

// New creates a read-only [io/fs.FS] backed by the given engine and resolver.
//
// The returned value also satisfies [io/fs.ReadDirFS] and [io/fs.StatFS].
func New(engine ReadEngine, resolver Resolver) fs.FS {
	return fsadapter.New(engine, resolver)
}

// NewWritable creates a [WritableFS] backed by the given engine and resolver.
//
// Write operations delegate to the engine's overlay layer.
func NewWritable(engine WriteEngine, resolver Resolver) WritableFS {
	return fsadapter.NewWritable(engine, resolver)
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
