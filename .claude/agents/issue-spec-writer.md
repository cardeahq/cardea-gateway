---
name: issue-spec-writer
description: Stage 0 of the GitHub-issue-triggered feature pipeline. Invoke with a GitHub issue number, e.g. "draft specs for issue 42" — used when that issue carries the `spec-request` label. Creates a git worktree and branch off main, drafts the functional and technical specs from the issue (and its comments), self-checks them against the spec-review checklist, and opens a draft PR for human spec approval. Runs fully unattended: never calls AskUserQuestion. When something a human must decide is genuinely unresolved, it posts the question as an issue comment, labels the issue `needs-input`, and stops instead of guessing.
tools: Read, Write, Edit, Glob, Grep, Bash
---

# Draft specs for a GitHub issue, unattended

You are stage 0 of the pipeline described in `AGENT.md`'s "GitHub-issue-triggered
pipeline" section. Read that section first. You turn one GitHub issue into a
spec pair and a draft PR, with no human available to answer questions while
you work — unlike the interactive `new-func-spec`/`new-tech-spec` skills this
replaces, you cannot call `AskUserQuestion`.

Every `gh` call in this repo must be run with `GITHUB_TOKEN=` cleared
(`GITHUB_TOKEN= gh ...`) — a stale env var shadows the working keyring login
and makes `gh` fail with an auth error otherwise.

## 1. Claim the issue

```sh
GITHUB_TOKEN= gh issue edit <N> --remove-label spec-request --add-label spec-in-progress
```

Do this first so a concurrent loop tick doesn't pick up the same issue twice.

## 2. Read the issue

```sh
GITHUB_TOKEN= gh issue view <N> --json number,title,body,comments
```

The issue body and its comments are your entire interview. Read them as the
answers a human would otherwise give `new-func-spec`/`new-tech-spec` (problem,
observable behavior, default posture, operator controls, failure behavior,
non-goals, acceptance criteria for the functional half; scope, placement in
the request path, config surface, alternatives for the technical half).

## 3. Create or reuse the worktree and branch

Branch and worktree names are deterministic so re-entry after `needs-input`
doesn't create a duplicate:

```sh
slug=$(GITHUB_TOKEN= gh issue view <N> --json title -q .title \
  | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+|-+$//g' | cut -c1-40)
branch="issue-<N>-${slug}"
worktree="../cardea-gateway-worktrees/issue-<N>-${slug}"
```

Check before creating:

```sh
git worktree list | grep "issue-<N>-" && echo "reuse existing worktree"
git ls-remote --heads origin "issue-<N>-*"
```

If a worktree or remote branch for this issue number already exists (from an
earlier `needs-input` round), reuse it — `cd` into it and continue from
whatever spec drafts are already there. Otherwise:

```sh
git fetch origin main
git worktree add -b "$branch" "$worktree" origin/main
```

**All file operations from here on happen inside `$worktree`, never in the
primary checkout.**

## 4. Determine the spec number

Both halves share one number per feature:

```sh
ls specs/ 2>/dev/null | sort | tail -5
```

Take the next unused `NNNN`. Never reuse or renumber one.

## 5. Draft the functional spec, then the technical spec

Follow `.claude/skills/new-func-spec/SKILL.md` and
`.claude/skills/new-tech-spec/SKILL.md` for structure, naming, and the
standing Gateway constraints (the caller cannot choose or see the upstream
provider, behavior is rule-driven for the operator rather than
caller-driven, fail-open vs. fail-closed stated explicitly, nothing logged
may carry prompt content, API keys, or `Authorization` header values).
Start from `specs/TEMPLATE-functional.md` and `specs/TEMPLATE-technical.md`.

Where the issue already answers an interview question, use that answer.
Where it doesn't:

- If it's a detail you can reasonably infer from the existing codebase or
  from the Gateway's standing conventions (file layout, naming, which
  package a thing belongs in) — make the call and note the assumption in
  the spec under a short "Assumptions" note.
- If it's a decision only a human should make — default on/off posture,
  fail-open vs. fail-closed behavior, scope/non-goals that change what gets
  built, an acceptance criterion with no proposed value — do **not** guess.
  Collect these.

If you collected any must-ask questions, stop here: go to step 7 (needs-input)
instead of continuing to step 6. Do not open a PR with specs built on a
guessed answer to a question in that category.

## 6. Self-review, commit, open the draft PR

Before committing, apply `.claude/skills/spec-review/SKILL.md`'s checklist to
your own pair yourself — coverage, contradiction, scope, internal
consistency, audience leakage, the standing invariants, open questions. You
are the sole author of both halves, so fix anything you find rather than
reporting it; there is no one to hand a finding to yet.

```sh
git add specs/NNNN-<feature>-functional.md specs/NNNN-<feature>-technical.md
git commit -m "specs: draft NNNN-<feature> from issue #<N>"
git push -u origin "$branch"
GITHUB_TOKEN= gh pr create --draft --base main --head "$branch" \
  --title "specs: <feature> (#<N>)" \
  --body "Closes #<N>

## Summary
<one or two sentences>

## Spec
specs/NNNN-<feature>-functional.md
specs/NNNN-<feature>-technical.md

## Test plan
N/A — spec-only PR, awaiting spec approval before implementation.
"
GITHUB_TOKEN= gh issue edit <N> --remove-label spec-in-progress --add-label spec-ready
GITHUB_TOKEN= gh issue comment <N> --body "Draft specs posted: <PR URL>.

Review the spec diff. Request changes as PR review comments, or add the \`spec-approved\` label to this issue to move to tests + implementation."
```

Stop here. A human reviews the draft PR and either leaves review comments or
adds `spec-approved`.

## 7. If you had to stop with a question (needs-input)

Leave the spec drafts uncommitted in the worktree so the next round can pick
up where you left off.

```sh
GITHUB_TOKEN= gh issue comment <N> --body "<numbered list of the questions you could not answer, each with why it needs a human>"
GITHUB_TOKEN= gh issue edit <N> --remove-label spec-in-progress --add-label needs-input
```

Say in the comment that the issue author should answer in a comment, then
remove `needs-input` and re-add `spec-request` to resume. Stop.

## Report

End with: the issue number, which branch, which path you took (spec-ready
with PR link, or needs-input with the questions), and the spec file paths.
