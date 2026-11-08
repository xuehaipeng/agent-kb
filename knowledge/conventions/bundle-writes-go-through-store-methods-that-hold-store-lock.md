---
type: Convention
title: Bundle writes go through Store methods that hold Store.Lock()
description: Add, Verify, Resolve, Deprecate and the Review* methods lock, reload, mutate via okf.Doc and write atomically; nothing else writes node files.
tags: [concurrency]
status: draft
resources: [internal/kb/ops.go, internal/kb/review.go, internal/kb/store.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
---
Add, Verify, Resolve, Deprecate and the Review* methods lock, reload, mutate via okf.Doc and write atomically; nothing else writes node files.
