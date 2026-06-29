package fusefs

import (
	"context"
	"io/fs"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fsadapter"
)

// ArtifactFS implements fs.FS, fs.ReadDirFS, and fs.StatFS, wrapping the
// existing Engine and Resolver for in-process use without FUSE.
//
// Symlink semantics: io/fs has no symlink support. Open on a symlink resolves
// the link target via the snapshot/overlay and returns the target's content.
// Stat reports fs.ModeSymlink on the file itself.
type ArtifactFS struct {
	inner *fsadapter.ArtifactFS
}

// NewArtifactFS creates a new fs.FS backed by the given Engine and Resolver.
func NewArtifactFS(engine *Engine, resolver *Resolver) *ArtifactFS {
	return &ArtifactFS{inner: fsadapter.New(engine, NewResolverAdapter(resolver))}
}

// WithContext returns a shallow copy of f that uses ctx for all I/O.
func (f *ArtifactFS) WithContext(ctx context.Context) fs.FS {
	return &ArtifactFS{inner: f.inner.WithContext(ctx)}
}

func (f *ArtifactFS) Open(name string) (fs.File, error)                { return f.inner.Open(name) }
func (f *ArtifactFS) ReadDir(name string) ([]fs.DirEntry, error)       { return f.inner.ReadDir(name) }
func (f *ArtifactFS) Stat(name string) (fs.FileInfo, error)            { return f.inner.Stat(name) }

// WritableFS extends fs.FS with write operations backed by the overlay.
type WritableFS interface {
	fs.FS
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	Mkdir(ctx context.Context, name string, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName string) error
}

type writableFS struct {
	inner fsadapter.WritableFS
}

// NewWritableFS wraps ArtifactFS with write capabilities.
func NewWritableFS(engine *Engine, resolver *Resolver) WritableFS {
	return &writableFS{inner: fsadapter.NewWritable(engine, NewResolverAdapter(resolver))}
}

func (w *writableFS) Open(name string) (fs.File, error)          { return w.inner.Open(name) }
func (w *writableFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	return w.inner.WriteFile(ctx, name, data, perm)
}
func (w *writableFS) Mkdir(ctx context.Context, name string, perm fs.FileMode) error {
	return w.inner.Mkdir(ctx, name, perm)
}
func (w *writableFS) Remove(ctx context.Context, name string) error {
	return w.inner.Remove(ctx, name)
}
func (w *writableFS) Rename(ctx context.Context, oldName, newName string) error {
	return w.inner.Rename(ctx, oldName, newName)
}

// resolverAdapter wraps *Resolver to convert []ReaddirEntry to []fsadapter.ReaddirEntry.
type resolverAdapter struct {
	r *Resolver
}

func (a *resolverAdapter) ResolvePath(path string) (fsadapter.ResolvedNode, error) {
	n, err := a.r.ResolvePath(path)
	if err != nil {
		return fsadapter.ResolvedNode{}, err
	}
	return fsadapter.ResolvedNode{
		FromOverlay: n.FromOverlay,
		Base:        n.Base,
		Overlay:     n.Overlay,
	}, nil
}

func (a *resolverAdapter) Getattr(path string) (mode uint32, size int64, nodeType string, mtime time.Time, ctime time.Time, err error) {
	return a.r.Getattr(path)
}

func (a *resolverAdapter) ReaddirTyped(ctx context.Context, path string) ([]fsadapter.ReaddirEntry, error) {
	entries, err := a.r.ReaddirTyped(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]fsadapter.ReaddirEntry, len(entries))
	for i, e := range entries {
		out[i] = fsadapter.ReaddirEntry{Name: e.Name, Type: e.Type}
	}
	return out, nil
}

// NewResolverAdapter wraps a fusefs.Resolver for use with fsadapter.New.
func NewResolverAdapter(r *Resolver) fsadapter.Resolver {
	return &resolverAdapter{r: r}
}
