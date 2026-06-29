package main

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fsadapter"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type snapshotStore interface {
	GetNode(int64, string) (model.BaseNode, bool)
	ListChildren(int64, string) ([]model.BaseNode, error)
}

type overlayStore interface {
	Get(string) (model.OverlayEntry, bool)
	ListByPrefix(context.Context, string) ([]model.OverlayEntry, error)
}

type exampleResolver struct {
	snap snapshotStore
	ov   overlayStore
	gen  int64
}

func (r *exampleResolver) ResolvePath(path string) (fsadapter.ResolvedNode, error) {
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

func (r *exampleResolver) Getattr(path string) (mode uint32, size int64, nodeType string, mtime, ctime time.Time, err error) {
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

func (r *exampleResolver) ReaddirTyped(ctx context.Context, path string) ([]fsadapter.ReaddirEntry, error) {
	set := map[string]fsadapter.ReaddirEntry{}
	children, err := r.snap.ListChildren(r.gen, path)
	if err == nil {
		for _, c := range children {
			name := filepath.Base(c.Path)
			set[name] = fsadapter.ReaddirEntry{Name: name, Type: c.Type}
		}
	}
	ovEntries, err := r.ov.ListByPrefix(ctx, path)
	if err == nil {
		for _, e := range ovEntries {
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
