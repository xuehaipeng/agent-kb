---
type: Convention
title: Bundle writes go through Store methods that hold Store.Lock()
description: Add, Update, Verify, Resolve, Deprecate, LinkProject and the Review* methods lock, reload, mutate via okf.Doc, write atomically and end in recordChange (log, reindex, hub autocommit).
tags: [concurrency]
status: stable
resources: [internal/kb/ops.go, internal/kb/review.go, internal/kb/store.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
revision: 2
updated: {by: claude-code, at: "2026-11-08T13:56:17Z", rev: 2, commit: a8425826899a, evidence: Update/LinkProject added; recordChange now reindexes and autocommits}
verified:
  - by: claude-code
    at: "2026-11-08T13:56:17Z"
    result: confirmed
    rev: 2
    commit: a8425826899a
    evidence: grep Lock() internal/kb/ops.go review.go hub.go; recordChange in store.go
---
Add, Verify, Resolve, Deprecate and the Review* methods lock, reload, mutate via okf.Doc and write atomically; nothing else writes node files.
