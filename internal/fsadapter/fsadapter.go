// Package fsadapter provides an io/fs.FS adapter that works against
// interface-typed dependencies, with no dependency on FUSE or SQLite.
//
// Use this package directly when you want the fs.FS layer without pulling in
// internal/fusefs (FUSE libraries) or the concrete store packages (SQLite).
// The fusefs and gitfs packages are thin wrappers around this adapter.
package fsadapter

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// ─── Interfaces ─────────────────────────────────────────────────────────────

// ReadEngine provides read access to file content.
type ReadEngine interface {
	Read(ctx context.Context, path string, off int64, size int) ([]byte, error)
}

// WriteEngine extends ReadEngine with write operations.
type WriteEngine interface {
	ReadEngine
	Write(ctx context.Context, path string, off int64, data []byte) (int, error)
	Mkdir(ctx context.Context, path string, mode uint32) error
	Unlink(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
}

// ResolvedNode is the result of path resolution.
type ResolvedNode struct {
	FromOverlay bool
	Base        model.BaseNode
	Overlay     model.OverlayEntry
}

// ReaddirEntry is a directory entry with name and type.
type ReaddirEntry struct {
	Name string
	Type string // "file", "dir", "symlink"
}

// Resolver resolves paths and provides directory/attribute information.
type Resolver interface {
	ResolvePath(path string) (ResolvedNode, error)
	Getattr(path string) (mode uint32, size int64, nodeType string, mtime time.Time, ctime time.Time, err error)
	ReaddirTyped(ctx context.Context, path string) ([]ReaddirEntry, error)
}

// ─── ArtifactFS ─────────────────────────────────────────────────────────────

// ArtifactFS implements fs.FS, fs.ReadDirFS, and fs.StatFS against the
// Engine and Resolver interfaces defined in this package.
type ArtifactFS struct {
	engine   ReadEngine
	resolver Resolver
	ctx      context.Context
}

// New creates a new fs.FS backed by the given engine and resolver.
func New(engine ReadEngine, resolver Resolver) *ArtifactFS {
	return &ArtifactFS{engine: engine, resolver: resolver, ctx: context.Background()}
}

// WithContext returns a shallow copy that uses ctx for all I/O.
func (f *ArtifactFS) WithContext(ctx context.Context) *ArtifactFS {
	return &ArtifactFS{engine: f.engine, resolver: f.resolver, ctx: ctx}
}

func (f *ArtifactFS) ctxValue() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func (f *ArtifactFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	node, err := f.resolver.ResolvePath(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	if isSymlink(node) {
		target := symlinkTarget(node)
		if target == "" {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
		}
		resolved, err := f.resolver.ResolvePath(target)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		node = resolved
	}
	if isDir(node) {
		return &artifactFile{fs: f, name: name, path: path, node: node, isDir: true}, nil
	}
	return &artifactFile{fs: f, name: name, path: path, node: node}, nil
}

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

func (f *ArtifactFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
	mode, size, nodeType, mtime, _, err := f.resolver.Getattr(path)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return &fileInfo{name: pathBase(path), size: size, mode: uint32ToFileMode(mode, nodeType), modTime: mtime}, nil
}

// ─── artifactFile ───────────────────────────────────────────────────────────

type artifactFile struct {
	fs     *ArtifactFS
	name   string
	path   string
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
	return &fileInfo{name: pathBase(f.path), size: size, mode: uint32ToFileMode(mode, nodeType), modTime: mtime}, nil
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

// ─── dirEntry / fileInfo / helpers ──────────────────────────────────────────

type dirEntry struct {
	name string
	typ  fs.FileMode
}

var _ fs.DirEntry = (*dirEntry)(nil)

func (d *dirEntry) Name() string             { return d.name }
func (d *dirEntry) Type() fs.FileMode        { return d.typ & fs.ModeType }
func (d *dirEntry) IsDir() bool              { return d.typ.IsDir() }
func (d *dirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }

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
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

func isDir(node ResolvedNode) bool {
	if node.FromOverlay {
		return node.Overlay.NodeType() == "dir"
	}
	return node.Base.Type == "dir"
}

func isSymlink(node ResolvedNode) bool {
	if node.FromOverlay {
		return node.Overlay.NodeType() == "symlink"
	}
	return node.Base.Type == "symlink"
}

func symlinkTarget(node ResolvedNode) string {
	if node.FromOverlay {
		return node.Overlay.TargetPath
	}
	return ""
}

// ─── WritableFS ─────────────────────────────────────────────────────────────

// WritableFS extends fs.FS with write operations.
type WritableFS interface {
	fs.FS
	WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error
	Mkdir(ctx context.Context, name string, perm fs.FileMode) error
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, oldName, newName string) error
}

type writableFS struct {
	*ArtifactFS
	engine WriteEngine
}

// NewWritable creates a WritableFS backed by the given engine and resolver.
func NewWritable(engine WriteEngine, resolver Resolver) WritableFS {
	return &writableFS{ArtifactFS: New(engine, resolver), engine: engine}
}

func (w *writableFS) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: "write", Path: name, Err: fs.ErrInvalid}
	}
	path := model.CleanPath(name)
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
