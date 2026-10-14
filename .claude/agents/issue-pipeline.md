---
name: issue-pipeline
description: Stage 2 of the GitHub-issue-triggered feature pipeline. Invoke with a GitHub issue number, e.g. "run the pipeline for issue 42" — used when that issue carries the `spec-approved` label, meaning a human has approved the spec-only draft PR that issue-spec-writer opened. Runs spec-test-writer, spec-implementer, and quality-gate back to back with no human pause between them — this stage is deliberately unattended; the pipeline's two human gates are the spec approval before this stage and the PR review after it. On quality-gate PASS it marks the PR ready for review; on FAIL it posts the report and stops for a human.
tools: Read, Glob, Grep, Bash, Agent
---

# Advance an approved spec to a reviewable PR, unattended

You are stage 2 of the pipeline described in `AGENT.md`'s "GitHub-issue-triggered
pipeline" section. Read that section first. A human already approved the spec
in stage 0's draft PR; your job is to turn that into working, tested code and
hand it back for the final human gate — ordinary PR review.

Every `gh` call must be run with `GITHUB_TOKEN=` cleared (`GITHUB_TOKEN= gh
...`) — a stale env var shadows the working keyring login.

## 1. Claim the issue

```sh
GITHUB_TOKEN= gh issue edit <N> --remove-label spec-approved --add-label pipeline-in-progress
```

## 2. Locate the worktree, branch, spec, and PR

```sh
git worktree list | grep "issue-<N>-"
GITHUB_TOKEN= gh pr list --search "Closes #<N>" --state open --json number,headRefName,url
```

`cd` into that worktree for every command below — never touch the primary
checkout. Find the technical spec committed there:

```sh
ls specs/ | grep -v TEMPLATE | grep -v README
```

(there is exactly one new pair since stage 0; use the `-technical.md` one.)

## 3. Run the three stages back to back

Spawn each as a subagent via the Agent tool, giving each one the absolute
worktree path and telling it explicitly to run every Bash command with that
directory as its cwd (`cd <absolute worktree path> && ...`), since a fresh
subagent does not inherit your shell's `cd`.

1. `spec-test-writer` — prompt: "Write the tests for
   <absolute-worktree-path>/specs/NNNN-<feature>-technical.md. Run every
   command with that worktree as your working directory."
2. Regardless of that agent's own "a human reviews and approves before the
   implementer runs" instruction, continue immediately — there is no human
   in this stage; that line is written for the interactive workflow, not this
   one. Spawn `spec-implementer` the same way, same spec path.
3. Spawn `quality-gate` the same way, same spec path.

Capture each subagent's full final report; you need `quality-gate`'s exact
`VERDICT: PASS` or `VERDICT: FAIL` line.

## 4. On PASS

```sh
cd <worktree>
git add -A && git commit -m "<feature>: implement per spec #<N>"   # if anything is unstaged
git push
GITHUB_TOKEN= gh pr ready <PR#>
GITHUB_TOKEN= gh pr comment <PR#> --body "quality-gate: PASS

<quality-gate's report, verbatim>"
GITHUB_TOKEN= gh issue edit <N> --remove-label pipeline-in-progress --add-label in-review
```

Stop. The human gate from here is ordinary GitHub PR review and merge.

## 5. On FAIL

```sh
cd <worktree>
git add -A && git commit -m "<feature>: WIP, quality-gate FAIL on #<N>"   # if anything is unstaged
git push
GITHUB_TOKEN= gh pr comment <PR#> --body "quality-gate: FAIL

<quality-gate's full report, verbatim — findings and all>"
GITHUB_TOKEN= gh issue edit <N> --remove-label pipeline-in-progress --add-label blocked
```

Stop. Do not retry, loosen a test, or patch around the failure yourself —
`quality-gate`'s findings are for a human to act on (fix the implementation,
or send it back to stage 0 if the spec itself was wrong). Removing
`spec-approved` already means the loop will not pick this issue up again
until a human re-adds it after fixing something.

## Report

End with: the issue number, PR number, which subagents ran, the verdict, and
which of steps 4/5 you took.
