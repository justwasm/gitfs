//go:build js || wasm

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudflare/artifact-fs/gitfs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func cloneAndBuildFSImpl(ctx context.Context) fs.FS {
	fmt.Println("(WASM: fetching tree via GitHub API)")

	owner, repo := parseGitHubURL(*repoURL)
	if owner == "" {
		log.Fatal("ERROR: --repo in WASM only supports github.com URLs")
	}

	commitSHA, err := githubResolveRef(ctx, owner, repo, *branch)
	if err != nil {
		log.Fatalf("resolve ref: %v", err)
	}
	fmt.Printf("HEAD: %s (%s)\n", commitSHA[:12], *branch)

	tree, err := githubGetTree(ctx, owner, repo, commitSHA)
	if err != nil {
		log.Fatalf("get tree: %v", err)
	}
	fmt.Printf("tree: %d entries\n", len(tree))

	snap := &memSnapshot{
		nodes:   map[string]model.BaseNode{},
		kids:    map[string][]model.BaseNode{},
		content: map[string][]byte{},
	}
	snap.addDir(".")

	for _, entry := range tree {
		path := entry.Path
		if path == "" {
			continue
		}
		mode := parseOctalMode(entry.Mode)

		switch entry.Type {
		case "blob":
			snap.nodes[path] = model.BaseNode{
				Path:      path,
				Type:      "file",
				Mode:      mode,
				ObjectOID: entry.SHA,
				SizeBytes: entry.Size,
			}
			dir := filepath.Dir(path)
			snap.kids[dir] = append(snap.kids[dir], snap.nodes[path])
		case "tree":
			snap.nodes[path] = model.BaseNode{Path: path, Type: "dir", Mode: mode}
			dir := filepath.Dir(path)
			snap.kids[dir] = append(snap.kids[dir], snap.nodes[path])
		}
	}

	ov := &memOverlay{entries: map[string]model.OverlayEntry{}}
	resolver := &memResolver{snap: snap, ov: ov, gen: 1}
	engine := &memEngine{
		snap:  snap,
		ov:    ov,
		gen:   1,
		files: map[string][]byte{},
		fetchBlob: &githubBlobFetcher{
			owner: owner,
			repo:  repo,
			cache: snap.content,
		},
	}

	fmt.Printf("snapshot: gen=1, %d files\n", len(snap.nodes))
	return gitfs.New(engine, resolver)
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
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", owner, repo, ref)
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

type githubTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

func githubGetTree(ctx context.Context, owner, repo, sha string) ([]githubTreeEntry, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, sha)
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
