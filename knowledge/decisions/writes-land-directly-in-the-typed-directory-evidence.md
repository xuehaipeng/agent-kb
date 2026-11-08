---
type: Decision
title: Writes land directly in the typed directory; evidence decides verified vs unverified
description: 'No inbox or consolidator: kb add writes the entry at once, verified when the author gives evidence, unverified otherwise; duplicates are caught at write time.'
tags: [write-path, design]
status: deprecated
resources: [internal/kb/ops.go]
supersedes: [decisions/the-unverified-trust-state-replaces-an-inbox-staging-area]
generated: {by: claude-code, at: "2026-11-08T13:56:16Z", commit: a8425826899a}
verified:
  - by: claude-code
    at: "2026-11-08T13:56:16Z"
    result: confirmed
    rev: 1
    commit: a8425826899a
    evidence: internal/kb/ops.go Add(); TestAddVerifyLifecycle duplicate check
---
Doc A's .inbox/ plus nightly consolidator would hide new knowledge from the agents who need it. Add rejects near-duplicates (Jaccard >= 0.85) and points at related entries (0.4-0.85) so agents update them instead of piling up variants.
