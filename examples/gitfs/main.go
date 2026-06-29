// Package main demonstrates using the gitfs package.
//
// Without flags it runs an in-memory demo (no git, no SQLite, WASM-safe).
// With --repo it clones the real repository and reads files through fs.FS.
//
// Usage:
//
//	go run ./examples/gitfs                                    # in-memory demo
//	go run ./examples/gitfs --repo https://github.com/justwasm/gitfs
//	go run ./examples/gitfs --repo https://github.com/golang/go --branch master
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/gitfs"
	"github.com/cloudflare/artifact-fs/internal/fsadapter"
	"github.com/cloudflare/artifact-fs/internal/model"
)

var (
	repoURL   = flag.String("repo", "", "git remote URL to clone (empty = in-memory demo)")
	branch    = flag.String("branch", "main", "branch to check out")
	corsPrefix = flag.String("cors-prefix", "", "CORS proxy prefix for WASM (e.g. https://no-cors.up.railway.app/)")
)

func main() {
	flag.Parse()
	ctx := context.Background()

	var fsys fs.FS
	if *repoURL == "" {
		fmt.Println("--- In-memory demo (no --repo specified) ---")
		fsys = buildInMemoryFS()
	} else {
		fmt.Printf("--- Cloning %s (%s) ---\n\n", *repoURL, *branch)
		fsys = cloneAndBuildFS(ctx)
	}

	if fsys == nil {
		log.Fatal("no filesystem available")
	}

	// ── Read root ───────────────────────────────────────────────────────

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

	// ── Walk ────────────────────────────────────────────────────────────

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
	fmt.Printf("\n  %d directories, %d files\n", dirCount, fileCount)

	// ── Read files ──────────────────────────────────────────────────────

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

	// ── Stat ────────────────────────────────────────────────────────────

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

	// ── WithContext ─────────────────────────────────────────────────────

	fmt.Println("\n--- Context ---")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	boundFS := gitfs.WithContext(fsys, ctx)
	data, err := fs.ReadFile(boundFS, samples[0])
	if err != nil {
		fmt.Printf("  %v\n", err)
	} else {
		fmt.Printf("  %s read with 5s timeout: %d bytes\n", samples[0], len(data))
	}
}

// ─── In-memory demo ─────────────────────────────────────────────────────────

func buildInMemoryFS() fs.FS {
	snap := &memSnapshot{nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{}, content: map[string][]byte{}}
	snap.addDir(".")
	snap.addDir("docs")
	snap.addFile("README.md", "# Hello from gitfs\n\nThis is an in-memory example.\n", 0o644)
	snap.addFile("docs/guide.md", "# Guide\n\nStep 1, step 2, step 3.\n", 0o644)
	snap.addFile("main.go", "package main\n\nfunc main() {}\n", 0o644)
	snap.addDir("src")
	snap.addFile("src/app.go", "package src\n\n// App does things.\n", 0o644)

	ov := &memOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := &memResolver{snap: snap, ov: ov, gen: 1}
	engine := &memEngine{snap: snap, ov: ov, gen: 1, files: map[string][]byte{}}
	return gitfs.New(engine, resolver)
}

// ─── Real clone ─────────────────────────────────────────────────────────────

func cloneAndBuildFS(ctx context.Context) fs.FS {
	// Lazy-imported to keep the in-memory path WASM-safe.
	// When --repo is set we accept the SQLite/FUSE dependency.
	return cloneAndBuildFSImpl(ctx)
}

// impl lives in a separate file behind a build tag so the in-memory
// example compiles for WASM without pulling in SQLite.
// See: clone_live.go

// ─── In-memory test doubles ─────────────────────────────────────────────────

type memSnapshot struct {
	nodes   map[string]model.BaseNode
	kids    map[string][]model.BaseNode
	content map[string][]byte
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
	children, err := r.snap.ListChildren(r.gen, path)
	if err == nil {
		for _, c := range children {
			name := filepath.Base(c.Path)
			set[name] = fsadapter.ReaddirEntry{Name: name, Type: c.Type}
		}
	}
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

type githubBlobFetcher struct {
	owner string
	repo  string
	cache map[string][]byte
}

func (f *githubBlobFetcher) fetch(ctx context.Context, oid string) ([]byte, error) {
	if data, ok := f.cache[oid]; ok {
		return data, nil
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/blobs/%s", f.owner, f.repo, oid)
	if *corsPrefix != "" {
		url = *corsPrefix + url
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github.v3.raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("blob %s: HTTP %d", oid[:12], resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	f.cache[oid] = data
	return data, nil
}

type memEngine struct {
	snap      *memSnapshot
	ov        *memOverlay
	gen       int64
	files     map[string][]byte
	fetchBlob *githubBlobFetcher // nil for in-memory demo
}

func (e *memEngine) Read(ctx context.Context, path string, off int64, size int) ([]byte, error) {
	// Check overlay writes first.
	if data, ok := e.files[path]; ok {
		return sliceChunk(data, off, size), nil
	}
	// Check snapshot content cache.
	if data, ok := e.snap.content[path]; ok {
		return sliceChunk(data, off, size), nil
	}
	// Lazy-fetch blob from remote (WASM clone path).
	if e.fetchBlob != nil {
		node, ok := e.snap.GetNode(e.gen, path)
		if ok && node.ObjectOID != "" {
			data, err := e.fetchBlob.fetch(ctx, node.ObjectOID)
			if err != nil {
				return nil, err
			}
			e.snap.content[path] = data
			return sliceChunk(data, off, size), nil
		}
	}
	return nil, fs.ErrNotExist
}

func sliceChunk(data []byte, off int64, size int) []byte {
	if off >= int64(len(data)) {
		return nil
	}
	end := off + int64(size)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return data[off:end]
}

func (e *memEngine) Write(_ context.Context, path string, off int64, data []byte) (int, error) {
	if e.files == nil {
		e.files = map[string][]byte{}
	}
	if _, exists := e.files[path]; !exists {
		if _, inSnap := e.snap.GetNode(e.gen, path); !inSnap {
			e.ov.entries[path] = model.OverlayEntry{
				Path:      path,
				Kind:      model.OverlayKindCreate,
				Mode:      0o644,
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
