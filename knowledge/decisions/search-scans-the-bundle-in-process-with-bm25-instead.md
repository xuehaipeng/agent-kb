---
type: Decision
title: Search scans the bundle in-process with BM25 instead of a SQLite FTS5 cache
description: At hundreds of files a full parse plus BM25 takes milliseconds and can never disagree with disk.
tags: [search, design]
status: deprecated
resources: [internal/kb/search.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
verified:
  - by: claude-code
    at: "2026-11-08T13:56:17Z"
    result: confirmed
    rev: 1
    commit: a8425826899a
    evidence: internal/kb/search.go Search() scans Nodes() in-process, no cache
---
Both input designs proposed a gitignored SQLite FTS5 index. It was dropped to avoid index/disk drift, WAL files, invalidation hooks and a heavy dependency on every host. Search is one function, so a persistent or embedding index can be added later. See docs/ARCHITECTURE.md section 2.3.
