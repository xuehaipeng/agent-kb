---
name: kb
description: Shared cross-agent knowledge base for this repo (kb CLI). Use before modifying code to look up verified facts, gotchas and decisions; to record what you learned; to verify other agents' claims; and to submit or perform cross-model code reviews.
---

# kb: shared knowledge and cross-review

The `kb` CLI manages `knowledge/`, a git-tracked set of small Markdown claims shared
by Claude Code, Codex, Hermes and other agents. Claims are *hypotheses* until a
different model family verifies them against the code.

## Retrieve (cheap, do this first)

```bash
kb search "timeout propagation" --file internal/gateway/stream.go   # ranked one-line headers
kb search --file internal/gateway/stream.go                        # everything anchored to a file
kb show learnings/stream-timeout-must-propagate                    # full claim + trust trail
kb search "..." --trusted                                          # only corroborated/reviewed
```

Badges: `corroborated` (another family confirmed), `reviewed` (human confirmed),
`unverified`, `disputed`, `stale` (past `stale_after`, or the files in
`resources` changed since it was last confirmed). Treat anything but
corroborated/reviewed as a lead to check, not a fact.

## Verify (cross-check another agent's claim)

Check the claim against the code or by running something, then:

```bash
kb verify <id> --confirm -e "cache_test.go:112 asserts the pin; go test ./pkg/... passes"
kb verify <id> --dispute -e "watermark.go no longer calls inc_lock_ref since a1b2c3"
```

Verifying your own family's claims is rejected by design. Disputed claims wait
for a human (`kb resolve`).

## Record

Record only durable, non-obvious knowledge: root causes, gotchas, invariants,
decisions and their reasons. Skip anything obvious from the code or the git log.

```bash
kb add learning "Vitest deadlocks on Node 24 without --no-isolate" \
  -d "Worker pool deadlocks on Node 24.1 unless isolation is disabled." \
  -r "vitest.config.ts" -t testing,vitest \
  -b "Repro: pnpm test on Node 24.1 hangs at 'RUN'. Fix: --no-isolate or pin Node 22."
kb add decision "Use Drizzle instead of Prisma" -d "..." --supersedes decisions/use-prisma
```

Types: `fact` (stable invariant), `learning` (gotcha/root cause; immutable),
`decision` (ADR; immutable), `convention` (team rule). Keep one claim per entry
and give `-r` globs for the code it depends on, which powers path search and
staleness. If `kb add` reports a duplicate, verify the existing claim instead.

## Cross-review loop

Author (after committing):

```bash
kb review submit "Refactor gRPC stream timeout propagation" \
  -m "Forward client ctx cancel to the upstream worker stream." \
  -a "Client cancel aborts the upstream stream immediately" --base main
```

Reviewer (must be a different model family):

```bash
kb review list --for-me
kb review show <id>          # packet, diffstat, invariants anchored to touched files
git diff <base>..<head>      # read the change; run targeted tests
kb review verdict <id> --approve -m "cancel path covered by stream_test.go"
kb review verdict <id> --changes -f "timeout_test.go never asserts upstream abort" -f "..."
```

Author after fixing: commit, then `kb review revise <id> -m "added abort assertion"`.
Judge the diff and the stated assumptions on their own; the packet deliberately
omits the author's reasoning so the review stays independent.

## Rules

- Never edit `knowledge/` files by hand; never store secrets, tokens or personal data.
- `kb status` shows disputes, stale claims and reviews waiting for you.
- Writes are attributed via `KB_ACTOR` (auto-detected for Claude Code); set
  `KB_ACTOR=<agent>/<model>` if `kb` asks.
