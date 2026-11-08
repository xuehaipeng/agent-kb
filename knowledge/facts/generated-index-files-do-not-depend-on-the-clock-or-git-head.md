---
type: Fact
title: Generated index files do not depend on the clock or git HEAD
description: IndexFiles uses TrustStatic (no stale flag), so two machines regenerate identical bytes and union merges stay clean.
tags: [index, determinism]
status: deprecated
resources: [internal/kb/index.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
stale_after: "2027-02-06T13:56:17Z"
verified:
  - by: claude-code
    at: "2026-11-08T13:56:17Z"
    result: confirmed
    rev: 1
    commit: a8425826899a
    evidence: IndexFiles uses TrustStatic; TestIndexDeterministic passes
---
Staleness is time- and HEAD-dependent, so it is shown only in search, show and status. TestIndexDeterministic covers this.
