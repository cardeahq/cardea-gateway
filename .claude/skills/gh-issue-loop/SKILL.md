---
name: gh-issue-loop
description: This skill should be used when asked to "run the issue loop", "check for GitHub tickets", "advance the pipeline", or when invoked on a schedule (a Claude Code cron job) to drive the GitHub-issue-triggered pipelines in cardeahq/cardea-gateway. One tick: pick up new `spec-request` issues (feature stage 0), advance `spec-approved` issues through tests/implementation/quality-gate (feature stage 2), fix `vulnerability` issues CI filed (vuln loop), report what's blocked or needs input, and clean up worktrees for merged PRs. Idempotent — safe to run repeatedly; a tick with nothing actionable is a normal, quiet result.
---

# Advance the GitHub-issue-triggered pipelines, one tick

This is the entry point described in `AGENT.md`'s "GitHub-issue-triggered
pipeline" and "Vulnerability loop" sections — read those first if you
haven't. It delegates the actual work to three agents, `issue-spec-writer`
and `issue-pipeline` (the feature pipeline's stages 0 and 2) and
`vuln-fixer` (the one-stage vuln loop); this skill's job is only to find
what's actionable, dispatch it, and report.

Every `gh` call must be run with `GITHUB_TOKEN=` cleared (`GITHUB_TOKEN= gh
...`) — a stale env var in some shells shadows the working keyring login and
makes every call fail with an auth error. If the very first call fails that
way, say so plainly and stop; don't guess at a fix.

## 1. Find actionable issues

```sh
GITHUB_TOKEN= gh issue list --label spec-request --state open --json number,title
GITHUB_TOKEN= gh issue list --label spec-approved --state open --json number,title
GITHUB_TOKEN= gh issue list --label vulnerability --state open --json number,title
```

These three labels are the only trigger conditions this skill acts on. Every
other pipeline label (`spec-in-progress`, `spec-ready`, `pipeline-in-progress`,
`vuln-in-progress`, `in-review`, `needs-input`, `blocked`) means something is
already in flight or waiting on a human — list them for the report, below,
but do not act on them:

```sh
GITHUB_TOKEN= gh issue list --label needs-input --state open --json number,title
GITHUB_TOKEN= gh issue list --label blocked --state open --json number,title
GITHUB_TOKEN= gh issue list --label spec-ready --state open --json number,title
GITHUB_TOKEN= gh issue list --label in-review --state open --json number,title
```

## 2. Dispatch

For every issue from the `spec-request` list, spawn the `issue-spec-writer`
agent with a prompt naming the issue number and title. For every issue from
the `spec-approved` list, spawn `issue-pipeline` the same way. For every
issue from the `vulnerability` list, spawn `vuln-fixer` the same way. If
there are several across any of these, spawn them together in one message —
they operate on independent worktrees and branches, so they don't conflict
with each other.

Each of these agents claims its own issue (swaps its trigger label for an
in-progress one) as its first step, so there's no need to de-duplicate here
beyond not spawning the same issue twice in one tick.

## 3. Clean up merged work (best effort)

```sh
GITHUB_TOKEN= gh pr list --state merged --json number,headRefName,mergedAt --limit 20
git worktree list
```

For any merged PR whose `headRefName` matches a worktree under
`../cardea-gateway-worktrees/`, remove it and the local branch:

```sh
git worktree remove ../cardea-gateway-worktrees/<dir>
git branch -D <branch>
```

Never remove a worktree for a PR that isn't merged, and never force-push or
delete a remote branch — GitHub's own "delete branch on merge" setting (if
enabled) or the human can do that. This step is cleanup, not part of the
pipeline's correctness; skip anything ambiguous rather than guessing.

## Report

One short summary: how many issues moved to stage 0, how many to stage 2,
how many vulnerability issues were dispatched to `vuln-fixer`, each agent's
one-line outcome, the counts still sitting in `needs-input` / `blocked` /
`spec-ready` / `in-review` (these need a human, not another tick), and any
worktrees cleaned up. If nothing was actionable, say that plainly in one
line — a quiet tick is the normal case, not a problem to explain away.
