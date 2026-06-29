package fusefs

import (
	"context"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// ─── Test doubles ───────────────────────────────────────────────────────────

type memSnapshot struct {
	nodes map[string]model.BaseNode
	kids  map[string][]model.BaseNode
}

func newMemSnapshot() *memSnapshot {
	return &memSnapshot{
		nodes: map[string]model.BaseNode{},
		kids:  map[string][]model.BaseNode{},
	}
}

func (m *memSnapshot) PublishGeneration(_ context.Context, _ string, _ string, _ []model.BaseNode) (int64, error) {
	return 1, nil
}
func (m *memSnapshot) GetNode(_ int64, path string) (model.BaseNode, bool) {
	n, ok := m.nodes[path]
	return n, ok
}
func (m *memSnapshot) ListChildren(_ int64, parentPath string) ([]model.BaseNode, error) {
	if v, ok := m.kids[parentPath]; ok {
		return v, nil
	}
	return nil, iofs.ErrNotExist
}

func (m *memSnapshot) addFile(path string, content []byte, mode uint32) string {
	if mode == 0 {
		mode = 0o644
	}
	oid := "oid-" + strings.ReplaceAll(path, "/", "-")
	n := model.BaseNode{
		Path:      path,
		Type:      "file",
		Mode:      mode,
		ObjectOID: oid,
		SizeBytes: int64(len(content)),
	}
	m.nodes[path] = n
	dir := filepath.Dir(path)
	if dir == "." {
		dir = "."
	}
	m.kids[dir] = append(m.kids[dir], n)
	return oid
}

func (m *memSnapshot) addDir(path string, mode uint32) {
	if mode == 0 {
		mode = 0o755
	}
	n := model.BaseNode{Path: path, Type: "dir", Mode: mode}
	m.nodes[path] = n
	if path == "." {
		return
	}
	dir := filepath.Dir(path)
	m.kids[dir] = append(m.kids[dir], n)
}

func (m *memSnapshot) addSymlink(path, target string) {
	n := model.BaseNode{Path: path, Type: "symlink", Mode: 0o777, SizeBytes: int64(len(target))}
	m.nodes[path] = n
	dir := filepath.Dir(path)
	m.kids[dir] = append(m.kids[dir], n)
}

// memOverlay uses temp files for BackingPath so Engine.Read works.
type memOverlay struct {
	entries map[string]model.OverlayEntry
	cleanup []string // temp file paths to remove
}

func newMemOverlay() *memOverlay {
	return &memOverlay{entries: map[string]model.OverlayEntry{}}
}

func (o *memOverlay) close() {
	for _, p := range o.cleanup {
		os.Remove(p)
	}
}

func (o *memOverlay) Get(path string) (model.OverlayEntry, bool) {
	e, ok := o.entries[path]
	return e, ok
}

func (o *memOverlay) ListByPrefix(_ context.Context, prefix string) ([]model.OverlayEntry, error) {
	if prefix == "" || prefix == "." {
		out := make([]model.OverlayEntry, 0, len(o.entries))
		for _, e := range o.entries {
			out = append(out, e)
		}
		return out, nil
	}
	out := []model.OverlayEntry{}
	for _, e := range o.entries {
		if strings.HasPrefix(e.Path, prefix+"/") || e.Path == prefix {
			out = append(out, e)
		}
	}
	return out, nil
}

func (o *memOverlay) EnsureCopyOnWrite(_ context.Context, _ model.RepoConfig, path string, base model.BaseNode) (model.OverlayEntry, error) {
	now := time.Now().UnixNano()
	e := model.OverlayEntry{
		Path:        model.CleanPath(path),
		Kind:        model.OverlayKindModify,
		Mode:        base.Mode,
		MtimeUnixNs: now,
		CtimeUnixNs: now,
		SourceOID:   base.ObjectOID,
	}
	o.entries[e.Path] = e
	return e, nil
}

func (o *memOverlay) CreateFile(_ context.Context, path string, mode uint32) (model.OverlayEntry, error) {
	now := time.Now().UnixNano()
	f, err := os.CreateTemp("", "afsovl-*")
	if err != nil {
		return model.OverlayEntry{}, err
	}
	f.Close()
	o.cleanup = append(o.cleanup, f.Name())
	e := model.OverlayEntry{
		Path:        model.CleanPath(path),
		Kind:        model.OverlayKindCreate,
		Mode:        mode,
		BackingPath: f.Name(),
		MtimeUnixNs: now,
		CtimeUnixNs: now,
	}
	o.entries[e.Path] = e
	return e, nil
}

func (o *memOverlay) WriteFile(_ context.Context, path string, off int64, data []byte) (int, error) {
	key := model.CleanPath(path)
	e, ok := o.entries[key]
	if !ok {
		return 0, iofs.ErrNotExist
	}
	bp := e.BackingPath
	if bp == "" {
		f, err := os.CreateTemp("", "afsovl-*")
		if err != nil {
			return 0, err
		}
		f.Close()
		bp = f.Name()
		o.cleanup = append(o.cleanup, bp)
		e.BackingPath = bp
		o.entries[key] = e
	}
	return writeFileAt(bp, off, data)
}

func writeFileAt(path string, off int64, data []byte) (int, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return f.Write(data)
}

func (o *memOverlay) Truncate(_ context.Context, _ string, _ int64) error { return nil }

func (o *memOverlay) Remove(_ context.Context, path string) error {
	o.entries[model.CleanPath(path)] = model.OverlayEntry{
		Path: model.CleanPath(path),
		Kind: model.OverlayKindDelete,
	}
	return nil
}

func (o *memOverlay) Rename(_ context.Context, oldPath, newPath string) error {
	oldPath = model.CleanPath(oldPath)
	newPath = model.CleanPath(newPath)
	e := o.entries[oldPath]
	delete(o.entries, oldPath)
	e.Path = newPath
	o.entries[newPath] = e
	return nil
}

func (o *memOverlay) RenameAndMarkModifiedFromBase(_ context.Context, oldPath, newPath string, sourceOID string) error {
	if err := o.Rename(context.Background(), oldPath, newPath); err != nil {
		return err
	}
	e := o.entries[model.CleanPath(newPath)]
	e.Kind = model.OverlayKindModify
	e.SourceOID = sourceOID
	o.entries[model.CleanPath(newPath)] = e
	return nil
}

func (o *memOverlay) Mkdir(_ context.Context, path string, mode uint32) error {
	now := time.Now().UnixNano()
	o.entries[model.CleanPath(path)] = model.OverlayEntry{
		Path: model.CleanPath(path), Kind: model.OverlayKindMkdir, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now,
	}
	return nil
}

func (o *memOverlay) SetMtime(_ context.Context, _ string, _ time.Time) error { return nil }

func (o *memOverlay) Reconcile(_ context.Context, _ func(string) (model.BaseNode, bool)) error {
	return nil
}

func (o *memOverlay) DirtyCount(_ context.Context) (int64, error) { return 0, nil }

type memHydrator struct {
	files map[string][]byte
}

func newMemHydrator() *memHydrator {
	return &memHydrator{files: map[string][]byte{}}
}

func (h *memHydrator) Enqueue(_ model.HydrationTask)        {}
func (h *memHydrator) EnqueueBatch(_ []model.HydrationTask) {}
func (h *memHydrator) QueueDepth(_ model.RepoID) int        { return 0 }
func (h *memHydrator) ReadBlob(_ context.Context, _ model.RepoConfig, _ model.BaseNode, _ int64) ([]byte, error) {
	return nil, nil
}

func (h *memHydrator) EnsureHydrated(_ context.Context, _ model.RepoConfig, node model.BaseNode) (string, int64, error) {
	if node.ObjectOID == "" {
		return "", 0, errors.New("no OID")
	}
	content, ok := h.files[node.ObjectOID]
	if !ok {
		return "", 0, errors.New("blob not found: " + node.ObjectOID)
	}
	f, err := os.CreateTemp("", "afstest-*")
	if err != nil {
		return "", 0, err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", 0, err
	}
	f.Close()
	return f.Name(), int64(len(content)), nil
}

// ─── Test helpers ───────────────────────────────────────────────────────────

type testFS struct {
	afs   *ArtifactFS
	ov    *memOverlay
	hydr  *memHydrator
	clean []string
}

func (t *testFS) close() {
	t.ov.close()
	for _, p := range t.clean {
		os.Remove(p)
	}
}

func buildTestFS(t *testing.T) *testFS {
	t.Helper()
	snap := newMemSnapshot()
	ov := newMemOverlay()
	hydr := newMemHydrator()

	snap.addDir(".", 0o755)
	snap.addDir("src", 0o755)
	snap.addDir("src/pkg", 0o755)
	oid1 := snap.addFile("README.md", []byte("hello world"), 0o644)
	oid2 := snap.addFile("src/main.go", []byte("package main"), 0o644)
	oid3 := snap.addFile("src/pkg/util.go", []byte("package pkg"), 0o644)
	snap.addSymlink("LINK", "README.md")

	hydr.files[oid1] = []byte("hello world")
	hydr.files[oid2] = []byte("package main")
	hydr.files[oid3] = []byte("package pkg")

	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(1)

	engine := &Engine{
		Resolver: resolver,
		Repo:     model.RepoConfig{ID: "test"},
		Overlay:  ov,
		Hydrator: hydr,
	}

	return &testFS{
		afs:  NewArtifactFS(engine, resolver),
		ov:   ov,
		hydr: hydr,
	}
}

// ─── Tests ──────────────────────────────────────────────────────────────────

func TestOpenRoot(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rdf, ok := file.(iofs.ReadDirFile)
	if !ok {
		t.Fatal("root Open should return ReadDirFile")
	}
	entries, err := rdf.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	want := []string{"LINK", "README.md", "src"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range names {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestOpenFile(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open("README.md")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("got %q, want %q", string(data), "hello world")
	}
}

func TestOpenDir(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open("src")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rdf, ok := file.(iofs.ReadDirFile)
	if !ok {
		t.Fatal("dir Open should return ReadDirFile")
	}
	entries, err := rdf.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	want := []string{"main.go", "pkg"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range names {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestReadDir(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	entries, err := ts.afs.ReadDir("src")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "main.go" || names[1] != "pkg" {
		t.Fatalf("got %v", names)
	}
}

func TestStat(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	fi, err := ts.afs.Stat("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Name() != "README.md" {
		t.Fatalf("name: got %q, want %q", fi.Name(), "README.md")
	}
	if fi.Size() != 11 {
		t.Fatalf("size: got %d, want 11", fi.Size())
	}
	if fi.IsDir() {
		t.Fatal("expected non-dir")
	}
}

func TestStatDir(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	fi, err := ts.afs.Stat("src")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatal("expected dir")
	}
}

func TestOpenBadPath(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	_, err := ts.afs.Open("/absolute")
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
	_, err = ts.afs.Open("README.md/..")
	if err == nil {
		t.Fatal("expected error for path with ..")
	}
}

func TestOpenNotExist(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	_, err := ts.afs.Open("nope.txt")
	if !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("got %v, want ErrNotExist", err)
	}
}

func TestOverlayDeleteHidesFile(t *testing.T) {
	snap := newMemSnapshot()
	ov := newMemOverlay()
	defer ov.close()
	hydr := newMemHydrator()

	snap.addDir(".", 0o755)
	oid := snap.addFile("a.txt", []byte("aaa"), 0o644)
	hydr.files[oid] = []byte("aaa")

	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{Resolver: resolver, Repo: model.RepoConfig{ID: "t"}, Overlay: ov, Hydrator: hydr}
	afs := NewArtifactFS(engine, resolver)

	ov.Remove(context.Background(), "a.txt")

	_, err := afs.Open("a.txt")
	if !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("got %v, want ErrNotExist", err)
	}
}

func TestOverlayReadPriority(t *testing.T) {
	snap := newMemSnapshot()
	ov := newMemOverlay()
	defer ov.close()
	hydr := newMemHydrator()

	snap.addDir(".", 0o755)
	oid := snap.addFile("f.txt", []byte("base"), 0o644)
	hydr.files[oid] = []byte("base")

	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{Resolver: resolver, Repo: model.RepoConfig{ID: "t"}, Overlay: ov, Hydrator: hydr}
	afs := NewArtifactFS(engine, resolver)

	// Promote to overlay.
	if err := engine.ensureOverlay(context.Background(), "f.txt"); err != nil {
		t.Fatal(err)
	}
	// Overwrite content via overlay.
	if _, err := ov.WriteFile(context.Background(), "f.txt", 0, []byte("overlay")); err != nil {
		t.Fatal(err)
	}
	// Update size.
	e := ov.entries["f.txt"]
	e.SizeBytes = 7
	ov.entries["f.txt"] = e

	f, err := afs.Open("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "overlay" {
		t.Fatalf("got %q, want %q", string(data), "overlay")
	}
}

func TestReadDirBatch(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open("src")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rdf := file.(iofs.ReadDirFile)
	e1, err := rdf.ReadDir(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(e1) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(e1))
	}
	e2, err := rdf.ReadDir(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(e2) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(e2))
	}
	_, err = rdf.ReadDir(1)
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestFileStat(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open("README.md")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Name() != "README.md" {
		t.Fatalf("got %q", fi.Name())
	}
	if fi.Size() != 11 {
		t.Fatalf("size: got %d, want 11", fi.Size())
	}
}

func TestClosedFileRead(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open("README.md")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	_, err = file.Read(make([]byte, 10))
	if err != iofs.ErrClosed {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

func TestReadFileFromSubdir(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	file, err := ts.afs.Open("src/pkg/util.go")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package pkg" {
		t.Fatalf("got %q, want %q", string(data), "package pkg")
	}
}

func TestFSTestMapFS(t *testing.T) {
	// Verify that fstest.TestFS itself works (sanity check).
	mapFS := fstest.MapFS{
		"hello.txt":     {Data: []byte("hello")},
		"dir/child.txt": {Data: []byte("child")},
	}
	if err := fstest.TestFS(mapFS, "hello.txt", "dir/child.txt"); err != nil {
		t.Fatalf("fstest.TestFS on MapFS failed: %v", err)
	}
}

func TestWritableFSWriteAndRead(t *testing.T) {
	snap := newMemSnapshot()
	ov := newMemOverlay()
	defer ov.close()
	hydr := newMemHydrator()

	snap.addDir(".", 0o755)
	snap.addFile("existing.txt", []byte("old"), 0o644)

	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{Resolver: resolver, Repo: model.RepoConfig{ID: "t"}, Overlay: ov, Hydrator: hydr}
	wfs := NewWritableFS(engine, resolver)

	ctx := context.Background()
	err := wfs.WriteFile(ctx, "new.txt", []byte("fresh"), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	f, err := wfs.Open("new.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh" {
		t.Fatalf("got %q, want %q", string(data), "fresh")
	}
}

func TestWritableFSMkdirAndRemove(t *testing.T) {
	snap := newMemSnapshot()
	ov := newMemOverlay()
	defer ov.close()
	hydr := newMemHydrator()

	snap.addDir(".", 0o755)

	resolver := &Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(1)
	engine := &Engine{Resolver: resolver, Repo: model.RepoConfig{ID: "t"}, Overlay: ov, Hydrator: hydr}
	wfs := NewWritableFS(engine, resolver)

	ctx := context.Background()
	err := wfs.Mkdir(ctx, "newdir", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	f, err := wfs.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rdf := f.(iofs.ReadDirFile)
	entries, err := rdf.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == "newdir" && e.IsDir() {
			found = true
		}
	}
	if !found {
		t.Fatal("mkdir did not appear in ReadDir")
	}
}

func TestRootDirEntryInfo(t *testing.T) {
	ts := buildTestFS(t)
	defer ts.close()
	entries, err := ts.afs.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		_, err := e.Info()
		if err != iofs.ErrInvalid {
			t.Fatalf("expected ErrInvalid for Info(), got %v", err)
		}
	}
}
