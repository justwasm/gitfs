# gitfs Example

Demonstrates the `gitfs` package with two modes: in-memory demo (no deps, WASM-safe) and live git clone.

## Usage

```bash
go run ./examples/gitfs                                                        # in-memory demo
go run ./examples/gitfs --repo https://github.com/justwasm/gitfs               # native clone
go run ./examples/gitfs --repo https://github.com/golang/go --branch master    # specific branch
go run ./examples/gitfs --repo URL --cors-prefix https://proxy.example.com/    # CORS proxy (WASM)
go run ./examples/gitfs --repo URL --persist                                   # SQLite-backed stores
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | _(empty)_ | Git remote URL. Empty = in-memory demo. |
| `--branch` | `main` | Branch to check out. |
| `--cors-prefix` | _(empty)_ | Prepend to GitHub API URLs for CORS proxy. |
| `--persist` | `false` | Use SQLite-backed snapshot/overlay stores (fallback to `:memory:` on WASM). |

## What's tested

| # | Flags | Mode | Storage |
|---|-------|------|---------|
| 1 | _(none)_ | In-memory | Go maps |
| 2 | `--repo` | Native git clone | SQLite file DB |
| 3 | `--repo --persist` | Native git clone | SQLite file DB |
| 4 | `--repo --persist` on WASM | GitHub API tree fetch | SQLite `:memory:` (fallback to Go maps) |
| 5 | `--repo --cors-prefix` on WASM | GitHub API via CORS proxy | Go maps |

## Capabilities demonstrated

| Capability | API |
|------------|-----|
| Directory listing | `fs.ReadDir` |
| Recursive walk | `fs.WalkDir` |
| File reading | `fs.ReadFile` (lazy blob fetch via HTTP on WASM) |
| File metadata | `fs.Stat` |
| Write file | `WritableFS.WriteFile` |
| Create directory | `WritableFS.Mkdir` |
| Context propagation | `gitfs.WithContext` |
| CORS proxy | `--cors-prefix` prepends to API URLs |
| SQLite fallback | `--persist` tries file DB → `:memory:` → Go maps |
| WASM | Compiles to `GOOS=js GOARCH=wasm`, no FUSE/CGo |

## Run

```bash
go run ./examples/gitfs
```

Requires `git` on `$PATH` for native clone mode. WASM mode only needs HTTP access to GitHub API.

## SQLite databases

`--persist` opens two SQLite databases:

| Database | Purpose | Schema |
|----------|---------|--------|
| Snapshot DB | Repository baseline: file tree, modes, OIDs, sizes | `repo_state` (HEAD OID, ref, generation) + `base_nodes` (path → metadata) |
| Overlay DB | User modifications: create, modify, delete, rename, mkdir, symlink | `overlay_entries` (path → kind, backing path, mode, timestamps) |

Read flow: overlay checked first (takes priority), then fallback to snapshot base nodes. This implements copy-on-write: a read-only snapshot + a mutable overlay layer.

On native, databases are file-based (`/tmp/gitfs-example.sqlite`). On WASM, file I/O fails and both fall back to `:memory:` automatically (logged via `slog` to stderr).
