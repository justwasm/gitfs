//go:build js || wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudflare/artifact-fs/gitfs"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

func cloneAndBuildFSImpl(ctx context.Context) fs.FS {
	owner, repo := parseGitHubURL(*repoURL)
	if owner == "" {
		log.Fatal("ERROR: --repo in WASM only supports github.com URLs")
	}

	// ── 1. Resolve HEAD (always needs GitHub API) ───────────────────────

	commitSHA, err := githubResolveRef(ctx, owner, repo, *branch)
	if err != nil {
		log.Fatalf("resolve ref: %v", err)
	}
	fmt.Printf("HEAD: %s (%s)\n", commitSHA[:12], *branch)

	// ── 2. Try loading from existing snapshot (if --persist) ────────────

	var snap snapshotStore
	var ov overlayStore
	var existingNodes []model.BaseNode

	if *persist {
		slog.Info("--persist: trying file-based SQLite")
		cfg := model.RepoConfig{
			ID: "example", Name: "example",
			MetaDBPath:    "/tmp/gitfs-example.sqlite",
			OverlayDBPath: "/tmp/gitfs-overlay.sqlite",
			OverlayDir:    "/tmp/gitfs-overlay-upper",
		}
		ss, err := snapshot.New(ctx, cfg.MetaDBPath)
		if err != nil {
			slog.Warn("snapshot.New failed", "err", err)
		} else {
			headOID, _, gen, err := ss.ReadState(ctx)
			if err == nil && headOID == commitSHA {
				slog.Info("snapshot cache hit", "gen", gen, "head", headOID[:12])
				snap = ss
			} else if err == nil {
				slog.Info("snapshot stale, will refresh", "stored", headOID[:12], "current", commitSHA[:12])
			} else {
				slog.Info("no existing snapshot")
			}
		}
		os, err := overlay.New(ctx, cfg)
		if err != nil {
			slog.Warn("overlay.New failed", "err", err)
		} else {
			slog.Info("overlay: OK")
			ov = os
		}
	}

	// ── 3. Fetch tree from GitHub API (if needed) ───────────────────────

	if snap == nil {
		fmt.Println("(fetching tree via GitHub API)")
		tree, err := githubGetTree(ctx, owner, repo, commitSHA)
		if err != nil {
			log.Fatalf("get tree: %v", err)
		}
		fmt.Printf("tree: %d entries\n", len(tree))

		existingNodes = treeToNodes(tree)

		if *persist && snap == nil {
			cfg := model.RepoConfig{
				ID: "example", Name: "example",
				MetaDBPath: "/tmp/gitfs-example.sqlite",
			}
			if ss, err := snapshot.New(ctx, cfg.MetaDBPath); err == nil {
				gen, err := ss.PublishGeneration(ctx, commitSHA, *branch, existingNodes)
				if err != nil {
					slog.Warn("PublishGeneration failed", "err", err)
				} else {
					slog.Info("snapshot published", "gen", gen, "nodes", len(existingNodes))
					snap = ss
				}
			}
		}
	} else {
		slog.Info("skipping GitHub API (snapshot cache hit)")
	}

	if snap == nil {
		if existingNodes == nil {
			// No cache, no --persist: fetch tree for in-memory.
			fmt.Println("(fetching tree via GitHub API)")
			tree, err := githubGetTree(ctx, owner, repo, commitSHA)
			if err != nil {
				log.Fatalf("get tree: %v", err)
			}
			fmt.Printf("tree: %d entries\n", len(tree))
			existingNodes = treeToNodes(tree)
		}
		snap = buildMemSnapshot(existingNodes)
	}
	if ov == nil {
		ov = &memOverlay{entries: map[string]model.OverlayEntry{}}
	}

	content := map[string][]byte{}
	resolver := &exampleResolver{snap: snap, ov: ov, gen: 1}
	engine := &memEngine{
		snap:  buildMemSnapshot(existingNodes),
		ov:    &memOverlay{entries: map[string]model.OverlayEntry{}},
		gen:   1,
		files: map[string][]byte{},
		fetchBlob: &githubBlobFetcher{
			owner: owner, repo: repo, cache: content,
		},
	}

	fmt.Printf("snapshot: gen=1, %d files\n", len(existingNodes))
	return gitfs.New(engine, resolver)
}

// ─── Helpers ────────────────────────────────────────────────────────────────

type githubTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

func treeToNodes(tree []githubTreeEntry) []model.BaseNode {
	nodes := []model.BaseNode{{Path: ".", Type: "dir", Mode: 0o755}}
	for _, e := range tree {
		if e.Path == "" {
			continue
		}
		mode := parseOctalMode(e.Mode)
		switch e.Type {
		case "blob":
			nodes = append(nodes, model.BaseNode{
				Path: e.Path, Type: "file", Mode: mode,
				ObjectOID: e.SHA, SizeBytes: e.Size,
			})
		case "tree":
			nodes = append(nodes, model.BaseNode{
				Path: e.Path, Type: "dir", Mode: mode,
			})
		}
	}
	return nodes
}

func buildMemSnapshot(nodes []model.BaseNode) *memSnapshot {
	snap := &memSnapshot{
		nodes: map[string]model.BaseNode{}, kids: map[string][]model.BaseNode{},
		content: map[string][]byte{},
	}
	snap.addDir(".")
	for _, n := range nodes {
		if n.Path == "." || n.Path == "" {
			continue
		}
		snap.nodes[n.Path] = n
		dir := filepath.Dir(n.Path)
		snap.kids[dir] = append(snap.kids[dir], n)
	}
	return snap
}

// ─── GitHub API ─────────────────────────────────────────────────────────────

func parseGitHubURL(raw string) (owner, repo string) {
	raw = strings.TrimSuffix(raw, ".git")
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimPrefix(raw, "github.com/")
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func githubResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	url := apiURL(fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", owner, repo, ref))
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API %d", resp.StatusCode)
	}
	var result struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Object.SHA, nil
}

func githubGetTree(ctx context.Context, owner, repo, sha string) ([]githubTreeEntry, error) {
	url := apiURL(fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, sha))
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API %d", resp.StatusCode)
	}
	var result struct {
		Tree []githubTreeEntry `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Tree, nil
}

func parseOctalMode(mode string) uint32 {
	if mode == "" {
		return 0o644
	}
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0o644
	}
	return uint32(v)
}

func apiURL(url string) string {
	if *corsPrefix != "" {
		return *corsPrefix + url
	}
	return url
}
