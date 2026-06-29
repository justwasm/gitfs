# gitfs Example

Demonstrates the `gitfs` package using in-memory stores — no FUSE, no SQLite, fully portable (compiles to WASM).

## Capabilities demonstrated

| Capability | API | What you see |
|------------|-----|--------------|
| Directory listing | `fs.ReadDir` | Root shows 4 entries, subdirectories expanded |
| Recursive walk | `fs.WalkDir` | 3 directories, 4 files traversed |
| File reading | `fs.ReadFile` | README.md content read back in full |
| File metadata | `fs.Stat` | size, mode, isDir reported |
| Write file | `WritableFS.WriteFile` | notes.txt written and read back |
| Create directory | `WritableFS.Mkdir` | drafts/ appears in subsequent ReadDir |
| Context propagation | `gitfs.WithContext` | Timeout-bounded read |
| Portable core | `GOOS=js GOARCH wasm` | No FUSE or SQLite dependency |

## Capabilities NOT demonstrated

This example uses in-memory stubs, so it does not exercise:

- **Hydrator blob fetch** — In production, `Engine.Read` calls `Hydrator.EnsureHydrated` to lazily pull blobs from the git object store on first access. The stub returns cached content directly.
- **Overlay copy-on-write** — The real overlay promotes a base file to a mutable copy before modification. The stub writes straight to a map.
- **Symlink resolution** — `Open` on a symlink resolves its target and returns the target's content. No symlinks are in the stub data.
- **Watcher / auto-refresh** — The daemon watches HEAD for changes and rebuilds the snapshot. This example is a one-shot snapshot.
- **FUSE mount** — The full `internal/fusefs` layer provides kernel-level VFS. This example is purely in-process.

## Run

```bash
go run ./examples/gitfs
go run ./examples/gitfs --repo https://github.com/golang/go --branch master
```

## Run for real

To read an actual git repository, replace the in-memory stubs with the concrete stores from `internal/snapshot`, `internal/overlay`, and `internal/hydrator`. See `internal/daemon/daemon.go` for the full initialization sequence:

1. `gitstore.CloneBlobless` — blobless clone
2. `gitstore.BuildTreeIndex` → `snapshot.PublishGeneration` — build snapshot
3. `overlay.New` — open overlay store
4. `hydrator.New` — create on-demand blob fetcher
5. `gitfs.New(engine, resolver)` — get an `fs.FS`
