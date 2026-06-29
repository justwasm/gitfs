# gitfs Example

Demonstrates using the `gitfs` package to clone a repository and read its contents through Go's standard `io/fs` interface — no FUSE mount required.

## What it does

1. Creates a temporary state directory for git objects, snapshots, and overlay data
2. Blobless-clones `https://github.com/justwasm/gitfs`
3. Builds a snapshot (tree index + blob sizes) into SQLite
4. Creates the overlay and hydrator (on-demand blob fetch)
5. Wraps everything in a `gitfs.New(engine, resolver)` → `fs.FS`
6. Uses standard library functions: `fs.ReadDir`, `fs.ReadFile`, `fs.Stat`, `fs.WalkDir`

## Run

```bash
go run ./examples/gitfs
```

Requires `git` on `$PATH. The first run clones the repo (blobless, no full history); subsequent runs reuse the cached `.git` directory.
