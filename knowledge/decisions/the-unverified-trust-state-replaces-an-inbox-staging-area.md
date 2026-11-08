---
type: Decision
title: The unverified trust state replaces an inbox staging area
description: kb add writes claims straight into the typed directory as unverified; duplicate control happens at write time.
tags: [write-path, design]
status: draft
resources: [internal/kb/ops.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
---
Doc A staged proposals in .inbox/ with a nightly consolidator. With the claims ledger, unverified already means staged, and hiding new claims would keep them from the agents who should verify them. Add rejects near-duplicates (Jaccard >= 0.85) and reports related claims (0.4-0.85). See docs/ARCHITECTURE.md section 2.4.
