---
type: Decision
title: Knowledge is grouped by project in one git-backed hub
description: ~/.kb holds projects/<id>/ plus global/; the project comes from the current repo's git remote; installs are user-wide so team repos stay untouched.
tags: [hub, projects, design]
status: stable
resources: [internal/kb/hub.go, internal/kb/store.go]
generated: {by: claude-code, at: "2026-11-08T13:56:16Z", commit: a8425826899a}
verified:
  - by: claude-code
    at: "2026-11-08T13:56:16Z"
    result: confirmed
    rev: 1
    commit: a8425826899a
    evidence: internal/kb/hub.go attachProject/projectForRepo; TestHubProjectsScopeAndStaleness, TestHubEndToEnd
---
The user works across many repos (skyfable-backend, aiproxy, kubedl, AtlasCloudTeam org repos). A per-repo knowledge/ dir needs team buy-in, has no home for cross-project knowledge, and makes Hermes clone every repo. The hub is one private repo: default search is the current project plus global entries that apply to it, --all spans projects, project.yaml maps repos (forks, frontend+backend) to a project, a gitignored registry finds local checkouts for staleness, and every write autocommits. Repo mode (knowledge/ inside a repo) still works and wins when present.
