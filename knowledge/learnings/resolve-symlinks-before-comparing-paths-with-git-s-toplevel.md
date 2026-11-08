---
type: Learning
title: Resolve symlinks before comparing paths with git's toplevel
description: git rev-parse --show-toplevel returns the resolved path (macOS /var -> /private/var), so filepath.Rel against an unresolved path yields ../.. and path-scoped checks silently skip.
tags: [macos, paths, git]
status: draft
resources: [internal/kb/store.go, internal/kb/git.go]
generated: {by: claude-code, at: "2026-11-08T10:33:19Z", commit: 11aeb9f53509}
stale_after: "2026-12-08T10:33:19Z"
---
Found via CI: TestImmutableStagedLint failed only on macos-latest because t.TempDir() lives under /var/folders, a symlink to /private/var. relToTop returned "" and the immutability lint skipped instead of failing. Open() and the git toplevel now go through filepath.EvalSymlinks (realPath), and kb tests open the repo through a symlink so Linux CI catches regressions.
