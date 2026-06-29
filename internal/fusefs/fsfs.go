package fusefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// ArtifactFS implements fs.FS, fs.ReadDirFS, and fs.StatFS, wrapping the
// existing Engine and Resolver for in-process use without FUSE.
//
// Symlink semantics: io/fs has no symlink support. Open on a symlink resolves
// the link target via the snapshot/overlay and returns the target's content.
// Stat reports fs.ModeSymlink on the file itself.
type ArtifactFS struct {
	engine   *Engine
	resolver *Resolver
	ctx      context.Context
}

// NewArtifactFS creates a new fs.FS backed by the given Engine and Resolver.
func NewArtifactFS(engine *Engine, resolver *Resolver) *ArtifactFS {
	return &ArtifactFS{
		engine:   engine,
		resolver: resolver,
		ctx:      context.Background(),
	}
}

// WithContext returns a shallow copy of f that uses ctx for all I/O.
func (f *ArtifactFS) WithContext(ctx context.Context) *ArtifactFS {
	return &ArtifactFS{
		engine:   f.engine,
		resolver: f.resolver,
		ctx:      ctx,
	}
}

func (f *ArtifactFS) ctxValue() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

// Open opens the named file. The name must be a valid io/fs path: relative,
// not starting with ".", not empty, and not containing "..".
//
// Root ("") or "." returns a directory listing of the filesystem root.
func (f *ArtifactFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	node, err := f.resolver.ResolvePath(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	// Symlink: resolve target so Open returns content.
	if f.isSymlink(node) {
		target := f.symlinkTarget(node)
		if target == "" {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
		}
		resolved, err := f.resolver.ResolvePath(target)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		node = resolved
	}
	if f.isDir(node) {
		return f.openDir(name, path, node)
	}
	return f.openFile(name, path, node), nil
}

func (f *ArtifactFS) openFile(name, path string, node ResolvedNode) *artifactFile {
	return &artifactFile{
		fs:   f,
		name: name,
		path: path,
		node: node,
	}
}

func (f *ArtifactFS) openDir(name, path string, node ResolvedNode) (*artifactFile, error) {
	af := &artifactFile{
		fs:   f,
		name: name,
		path: path,
		node: node,
		isDir: true,
	}
	return af, nil
}

// ReadDir reads the named directory and returns its directory entries.
func (f *ArtifactFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	entries, err := f.resolver.ReaddirTyped(f.ctxValue(), path)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, len(entries))
	for i, e := range entries {
		out[i] = &dirEntry{name: e.Name, typ: typeToFileMode(e.Type)}
	}
	return out, nil
}

// Stat returns a FileInfo for the named path.
func (f *ArtifactFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	mode, size, nodeType, mtime, _, err := f.resolver.Getattr(path)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	// Getattr normalizes mode to include permission bits; convert to fs.FileMode.
	fm := uint32ToFileMode(mode, nodeType)
	base := pathBase(path)
	return &fileInfo{name: base, size: size, mode: fm, modTime: mtime}, nil
}

func (f *ArtifactFS) isDir(node ResolvedNode) bool {
	if node.FromOverlay {
		return node.Overlay.NodeType() == "dir"
	}
	return node.Base.Type == "dir"
}

func (f *ArtifactFS) isSymlink(node ResolvedNode) bool {
	if node.FromOverlay {
		return node.Overlay.NodeType() == "symlink"
	}
	return node.Base.Type == "symlink"
}

func (f *ArtifactFS) symlinkTarget(node ResolvedNode) string {
	if node.FromOverlay {
		return node.Overlay.TargetPath
	}
	// Base symlinks store the target in ObjectOID — but actually, for base
	// nodes we don't store the link target. We can only resolve symlinks
	// stored in the overlay. For base-only symlinks, we'd need to hydrate
	// and read the blob. For now, return empty if not overlay.
	//
	// TODO: For base symlinks, hydrate the blob and read the link target.
	// This requires calling Hydrator.ReadBlob which returns content.
	return ""
}

// ─── artifactFile ───────────────────────────────────────────────────────────

type artifactFile struct {
	fs     *ArtifactFS
	name   string // original io/fs name for errors
	path   string // CleanPath internal path
	node   ResolvedNode
	mu     sync.Mutex
	off    int64
	closed bool
	isDir  bool
}

var (
	_ fs.File        = (*artifactFile)(nil)
	_ fs.ReadDirFile = (*artifactFile)(nil)
)

func (f *artifactFile) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	mode, size, nodeType, mtime, _, err := f.fs.resolver.Getattr(f.path)
	if err != nil {
		return nil, err
	}
	fm := uint32ToFileMode(mode, nodeType)
	base := pathBase(f.path)
	return &fileInfo{name: base, size: size, mode: fm, modTime: mtime}, nil
}

func (f *artifactFile) Read(buf []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.isDir {
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: errors.New("is a directory")}
	}
	data, err := f.fs.engine.Read(f.fs.ctxValue(), f.path, f.off, len(buf))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, err
	}
	n := copy(buf, data)
	f.off += int64(n)
	if n < len(buf) {
		return n, io.EOF
	}
	return n, nil
}

func (f *artifactFile) Close() error {
	if f.closed {
		return fs.ErrClosed
	}
	f.closed = true
	return nil
}

func (f *artifactFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	if !f.isDir {
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: errors.New("not a directory")}
	}
	entries, err := f.fs.resolver.ReaddirTyped(f.fs.ctxValue(), f.path)
	if err != nil {
		return nil, err
	}
	// Apply offset for continued reads.
	if f.off > 0 && f.off < int64(len(entries)) {
		entries = entries[f.off:]
	} else if f.off >= int64(len(entries)) {
		return nil, io.EOF
	}
	var out []fs.DirEntry
	if n <= 0 {
		out = make([]fs.DirEntry, len(entries))
		for i, e := range entries {
			out[i] = &dirEntry{name: e.Name, typ: typeToFileMode(e.Type)}
		}
		f.off = int64(len(entries))
	} else {
		end := n
		if end > len(entries) {
			end = len(entries)
		}
		out = make([]fs.DirEntry, end)
		for i := 0; i < end; i++ {
			out[i] = &dirEntry{name: entries[i].Name, typ: typeToFileMode(entries[i].Type)}
		}
		f.off += int64(end)
	}
	return out, nil
}

// ─── dirEntry ───────────────────────────────────────────────────────────────

type dirEntry struct {
	name string
	typ  fs.FileMode
}

var _ fs.DirEntry = (*dirEntry)(nil)

func (d *dirEntry) Name() string               { return d.name }
func (d *dirEntry) Type() fs.FileMode           { return d.typ & fs.ModeType }
func (d *dirEntry) IsDir() bool                 { return d.typ.IsDir() }
func (d *dirEntry) Info() (fs.FileInfo, error)   { return nil, fs.ErrInvalid }

// ─── fileInfo ───────────────────────────────────────────────────────────────

type fileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

var _ fs.FileInfo = (*fileInfo)(nil)

func (fi *fileInfo) Name() string      { return fi.name }
func (fi *fileInfo) Size() int64       { return fi.size }
func (fi *fileInfo) Mode() fs.FileMode { return fi.mode }
func (fi *fileInfo) ModTime() time.Time { return fi.modTime }
func (fi *fileInfo) IsDir() bool       { return fi.mode.IsDir() }
func (fi *fileInfo) Sys() any          { return nil }

// ─── helpers ────────────────────────────────────────────────────────────────

func typeToFileMode(typ string) fs.FileMode {
	switch typ {
	case "dir":
		return fs.ModeDir
	case "symlink":
		return fs.ModeSymlink
	default:
		return 0
	}
}

func uint32ToFileMode(mode uint32, typ string) fs.FileMode {
	fm := fs.FileMode(mode & 0o777)
	switch typ {
	case "dir":
		fm |= fs.ModeDir
	case "symlink":
		fm |= fs.ModeSymlink
	}
	return fm
}

func pathBase(path string) string {
	if path == "." {
		return "."
	}
	// Find last segment after "/".
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// ─── WritableFS ─────────────────────────────────────────────────────────────

// WritableFS extends fs.FS with write operations backed by the overlay.
type WritableFS interface {
	fs.FS
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	Mkdir(ctx context.Context, name string, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName string) error
}

var _ WritableFS = (*writableFS)(nil)

type writableFS struct {
	*ArtifactFS
}

// NewWritableFS wraps ArtifactFS with write capabilities.
func NewWritableFS(engine *Engine, resolver *Resolver) WritableFS {
	return &writableFS{ArtifactFS: NewArtifactFS(engine, resolver)}
}

func (w *writableFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "write", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	// Create or overlay-promote then write.
	if _, err := w.engine.Write(ctx, path, 0, data); err != nil {
		return &fs.PathError{Op: "write", Path: name, Err: err}
	}
	return nil
}

func (w *writableFS) Mkdir(ctx context.Context, name string, perm fs.FileMode) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	if err := w.engine.Mkdir(ctx, path, uint32(perm)); err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	return nil
}

func (w *writableFS) Remove(ctx context.Context, name string) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	if path == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	if err := w.engine.Unlink(ctx, path); err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	return nil
}

func (w *writableFS) Rename(ctx context.Context, oldName, newName string) error {
	if !fs.ValidPath(oldName) || !fs.ValidPath(newName) {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrInvalid}
	}
	oldPath := model.CleanPath(oldName)
	newPath := model.CleanPath(newName)
	if err := w.engine.Rename(ctx, oldPath, newPath); err != nil {
		return &fs.PathError{Op: "rename", Path: oldName, Err: err}
	}
	return nil
}
