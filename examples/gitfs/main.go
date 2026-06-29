// Package main demonstrates using the gitfs package with in-memory stores.
//
// It shows how to build an fs.FS from snapshot + overlay data without FUSE
// or SQLite. For a full example with real git clone, see the daemon.
//
// Usage:
//
//	go run ./examples/gitfs
//	go run ./examples/gitfs --repo https://github.com/golang/go --branch master
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/gitfs"
	"github.com/cloudflare/artifact-fs/internal/fsadapter"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var (
	repoURL = flag.String("repo", "https://github.com/justwasm/gitfs", "git remote URL (informational)")
	branch  = flag.String("branch", "main", "branch name (informational)")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	// ── 1. Build an in-memory snapshot ───────────────────────────────────

	snap := &memSnapshot{nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{}, content: map[string][]byte{}}
	snap.addDir(".")
	snap.addDir("docs")
	snap.addFile("README.md", "# Hello from gitfs\n\nThis is an in-memory example.\n", 0o644)
	snap.addFile("docs/guide.md", "# Guide\n\nStep 1, step 2, step 3.\n", 0o644)
	snap.addFile("main.go", "package main\n\nfunc main() {}\n", 0o644)
	snap.addDir("src")
	snap.addFile("src/app.go", "package src\n\n// App does things.\n", 0o644)

	ov := &memOverlay{entries: map[string]model.OverlayEntry{}}

	// ── 2. Create resolver + engine (all in-memory) ─────────────────────

	// In real usage you'd implement these interfaces against your DB.
	// Here we use the adapter interfaces directly — no SQLite, no FUSE.
	resolver := &memResolver{snap: snap, ov: ov, gen: 1}
	engine := &memEngine{snap: snap, ov: ov, gen: 1, files: map[string][]byte{}}

	fsys := gitfs.New(engine, resolver)

	// ── 3. Read the root directory ───────────────────────────────────────

	fmt.Println("--- Root ---")
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

	// ── 4. Walk the tree ─────────────────────────────────────────────────

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
		fmt.Printf("  %s\n", path)
		return nil
	})
	fmt.Printf("\n  %d directories, %d files\n", dirCount, fileCount)

	// ── 5. Read some files ───────────────────────────────────────────────

	fmt.Println("\n--- Read ---")
	for _, name := range []string{"README.md", "docs/guide.md", "src/app.go"} {
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

	// ── 6. Stat ──────────────────────────────────────────────────────────

	fmt.Println("--- Stat ---")
	for _, name := range []string{".", "README.md", "docs"} {
		fi, err := fs.Stat(fsys, name)
		if err != nil {
			fmt.Printf("  %s: %v\n", name, err)
			continue
		}
		fmt.Printf("  %s: size=%d mode=%s isDir=%v\n", name, fi.Size(), fi.Mode(), fi.IsDir())
	}

	// ── 7. WritableFS ────────────────────────────────────────────────────

	fmt.Println("\n--- Write ---")
	wfs := gitfs.NewWritable(engine, resolver)

	if err := wfs.WriteFile(ctx, "notes.txt", []byte("hello from gitfs\n"), 0o644); err != nil {
		log.Fatalf("WriteFile: %v", err)
	}
	if err := wfs.Mkdir(ctx, "drafts", 0o755); err != nil {
		log.Fatalf("Mkdir: %v", err)
	}

	data, err := fs.ReadFile(wfs, "notes.txt")
	if err != nil {
		log.Fatalf("ReadFile after write: %v", err)
	}
	fmt.Printf("  notes.txt: %s", data)

	// Read back via ReadDir to confirm drafts/ appeared.
	entries, _ = fs.ReadDir(wfs, ".")
	fmt.Print("  root after writes: ")
	for _, e := range entries {
		fmt.Printf("%s ", e.Name())
	}
	fmt.Println()

	// ── 8. WithContext ───────────────────────────────────────────────────

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fsys = gitfs.WithContext(fsys, ctx)

	data, err = fs.ReadFile(fsys, "README.md")
	if err != nil {
		log.Fatalf("ReadFile with context: %v", err)
	}
	fmt.Printf("\n  README.md with context: %d bytes\n", len(data))

	_ = *repoURL // informational only
	_ = *branch
}

// ─── In-memory test doubles (no SQLite, no FUSE) ────────────────────────────

type memSnapshot struct {
	nodes   map[string]model.BaseNode
	kids    map[string][]model.BaseNode
	content map[string][]byte // file path → content
}

func (m *memSnapshot) addDir(path string) {
	m.nodes[path] = model.BaseNode{Path: path, Type: "dir", Mode: 0o755}
	if path != "." {
		dir := filepath.Dir(path)
		m.kids[dir] = append(m.kids[dir], m.nodes[path])
	}
}

func (m *memSnapshot) addFile(path, content string, mode uint32) {
	m.nodes[path] = model.BaseNode{Path: path, Type: "file", Mode: mode, SizeBytes: int64(len(content))}
	m.content[path] = []byte(content)
	dir := filepath.Dir(path)
	m.kids[dir] = append(m.kids[dir], m.nodes[path])
}

func (m *memSnapshot) GetNode(_ int64, path string) (model.BaseNode, bool) {
	n, ok := m.nodes[path]
	return n, ok
}

func (m *memSnapshot) ListChildren(_ int64, parent string) ([]model.BaseNode, error) {
	if v, ok := m.kids[parent]; ok {
		return v, nil
	}
	return nil, fs.ErrNotExist
}

type memOverlay struct {
	entries map[string]model.OverlayEntry
}

func (o *memOverlay) Get(path string) (model.OverlayEntry, bool) {
	e, ok := o.entries[path]
	return e, ok
}

func (o *memOverlay) ListByPrefix(_ context.Context, _ string) ([]model.OverlayEntry, error) {
	return nil, nil
}

// ─── Resolver (satisfies gitfs.Resolver / fsadapter.Resolver) ───────────────

type memResolver struct {
	snap *memSnapshot
	ov   *memOverlay
	gen  int64
}

func (r *memResolver) ResolvePath(path string) (fsadapter.ResolvedNode, error) {
	if ov, ok := r.ov.Get(path); ok {
		if ov.IsDeleted() {
			return fsadapter.ResolvedNode{}, fs.ErrNotExist
		}
		return fsadapter.ResolvedNode{FromOverlay: true, Overlay: ov}, nil
	}
	if n, ok := r.snap.GetNode(r.gen, path); ok {
		return fsadapter.ResolvedNode{Base: n}, nil
	}
	return fsadapter.ResolvedNode{}, fs.ErrNotExist
}

func (r *memResolver) Getattr(path string) (mode uint32, size int64, nodeType string, mtime, ctime time.Time, err error) {
	n, err2 := r.ResolvePath(path)
	if err2 != nil {
		return 0, 0, "", time.Time{}, time.Time{}, err2
	}
	if n.FromOverlay {
		return n.Overlay.Mode, n.Overlay.SizeBytes, n.Overlay.NodeType(),
			time.Unix(0, n.Overlay.MtimeUnixNs), time.Unix(0, n.Overlay.CtimeUnixNs), nil
	}
	m := n.Base.Mode & 0o777
	if n.Base.Type == "dir" && m == 0 {
		m = 0o755
	}
	if (n.Base.Type == "file" || n.Base.Type == "symlink") && m == 0 {
		m = 0o644
	}
	return m, n.Base.SizeBytes, n.Base.Type, time.Now(), time.Now(), nil
}

func (r *memResolver) ReaddirTyped(_ context.Context, path string) ([]fsadapter.ReaddirEntry, error) {
	set := map[string]fsadapter.ReaddirEntry{}
	// Snapshot children.
	children, err := r.snap.ListChildren(r.gen, path)
	if err == nil {
		for _, c := range children {
			name := filepath.Base(c.Path)
			set[name] = fsadapter.ReaddirEntry{Name: name, Type: c.Type}
		}
	}
	// Overlay entries (create/mkdir override snapshot).
	for _, e := range r.ov.entries {
		name, ok := childName(path, e.Path)
		if !ok {
			continue
		}
		if e.IsDeleted() {
			delete(set, name)
			continue
		}
		set[name] = fsadapter.ReaddirEntry{Name: name, Type: e.NodeType()}
	}
	out := make([]fsadapter.ReaddirEntry, 0, len(set))
	for _, e := range set {
		out = append(out, e)
	}
	return out, nil
}

func childName(parent, entryPath string) (string, bool) {
	var rel string
	if parent == "." {
		rel = entryPath
	} else {
		var ok bool
		rel, ok = strings.CutPrefix(entryPath, parent+"/")
		if !ok {
			return "", false
		}
	}
	if rel == "" {
		return "", false
	}
	rel, _, _ = strings.Cut(rel, "/")
	return rel, true
}

// ─── Engine (satisfies gitfs.WriteEngine / fsadapter.WriteEngine) ──────────

type memEngine struct {
	snap  *memSnapshot
	ov    *memOverlay
	gen   int64
	files map[string][]byte // overlay content (written files)
}

func (e *memEngine) Read(_ context.Context, path string, off int64, size int) ([]byte, error) {
	// Check overlay first (written files).
	if data, ok := e.files[path]; ok {
		if off >= int64(len(data)) {
			return nil, io.EOF
		}
		end := off + int64(size)
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return data[off:end], nil
	}
	// Check snapshot content cache.
	if data, ok := e.snap.content[path]; ok {
		if off >= int64(len(data)) {
			return nil, io.EOF
		}
		end := off + int64(size)
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		return data[off:end], nil
	}
	return nil, fs.ErrNotExist
}

func (e *memEngine) Write(_ context.Context, path string, off int64, data []byte) (int, error) {
	if e.files == nil {
		e.files = map[string][]byte{}
	}
	// Create overlay entry for new files so ReaddirTyped sees them.
	if _, exists := e.files[path]; !exists {
		if _, inSnap := e.snap.GetNode(e.gen, path); !inSnap {
			e.ov.entries[path] = model.OverlayEntry{
				Path:     path,
				Kind:     model.OverlayKindCreate,
				Mode:     0o644,
				SizeBytes: off + int64(len(data)),
			}
		}
	}
	f := e.files[path]
	end := off + int64(len(data))
	if int64(len(f)) < end {
		grown := make([]byte, end)
		copy(grown, f)
		f = grown
	}
	copy(f[off:], data)
	e.files[path] = f
	return len(data), nil
}

func (e *memEngine) Mkdir(_ context.Context, path string, mode uint32) error {
	e.ov.entries[path] = model.OverlayEntry{Path: path, Kind: model.OverlayKindMkdir, Mode: mode}
	return nil
}

func (e *memEngine) Unlink(_ context.Context, path string) error {
	e.ov.entries[path] = model.OverlayEntry{Path: path, Kind: model.OverlayKindDelete}
	return nil
}

func (e *memEngine) Rename(_ context.Context, _, _ string) error { return nil }
