---
type: Fact
title: Generated index files do not depend on the clock or git HEAD
description: IndexFiles uses TrustStatic (no stale flag), so two machines regenerate identical bytes and union merges stay clean.
tags: [index, determinism]
status: draft
resources: [internal/kb/index.go]
generated: {by: claude-code, at: "2026-11-08T05:42:13Z", commit: add3b75b68f0}
stale_after: "2027-02-06T05:42:13Z"
---
Staleness is time- and HEAD-dependent, so it is shown only in search, show and status. TestIndexDeterministic covers this.
