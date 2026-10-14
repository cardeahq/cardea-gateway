---
name: vuln-fixer
description: Fixes a GitHub issue labeled `vulnerability` — one filed automatically by CI when the `govulncheck` job fails on main. Invoke with the issue number, e.g. "fix vulnerability issue 42". Creates a worktree and branch off main, bumps whichever module or Go toolchain version clears each reachable finding, verifies with `make check` and a clean `govulncheck` re-run, and opens a PR directly — there is no spec stage for this, since the fix is mechanical. Runs unattended; the only human gate is the resulting PR's review.
tools: Read, Glob, Grep, Bash
---

# Fix a govulncheck finding, unattended

One stage, no spec: a vulnerability fix is "bump a version and verify", not a
design decision, so this agent goes straight from issue to PR. The single
human gate is the PR review — same as every other PR in this repo.

Every `gh` call must be run with `GITHUB_TOKEN=` cleared (`GITHUB_TOKEN= gh
...`) — a stale env var shadows the working keyring login.

## 1. Claim the issue

```sh
GITHUB_TOKEN= gh issue edit <N> --remove-label vulnerability --add-label vuln-in-progress
```

## 2. Worktree and branch

Same convention as the feature pipeline (`AGENT.md`, "GitHub-issue-triggered
pipeline"):

```sh
slug="govulncheck-fix"
branch="issue-<N>-${slug}"
worktree="../cardea-gateway-worktrees/issue-<N>-${slug}"
git worktree list | grep "issue-<N>-" || { git fetch origin main; git worktree add -b "$branch" "$worktree" origin/main; }
```

Reuse an existing worktree/branch for this issue number if one is already
there. All commands from here on run with `$worktree` as the working
directory.

## 3. Find what's actually reachable, and what fixes it

The issue body has `govulncheck`'s human-readable output, but re-run it
yourself with `-json` and extract findings precisely rather than parsing
prose — "affected" (reachable) findings only, each mapped to its fixing
module version or Go version:

```sh
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck -json ./... > /tmp/govulncheck.json

jq -s '
  ([.[] | select(.finding != null) | .finding.osv] | unique) as $ids
  | [.[] | select(.osv != null and (.osv.id as $i | $ids | index($i) != null))
     | {id: .osv.id, summary: .osv.summary,
        modules: [.osv.affected[].package.name] | unique,
        fixed: [.osv.affected[].ranges[]?.events[]? | select(.fixed != null) | .fixed] | unique}]
' /tmp/govulncheck.json
```

Each result's `modules` tells you the fix:

- **A real module path** (e.g. `golang.org/x/text`) — bump it:
  `go get <module>@<lowest fixed version that clears it>`, then
  `go mod tidy`.
- **`"stdlib"`** — the Go toolchain itself needs bumping. Use the lowest
  `fixed` version matching this repo's current major.minor line (e.g. if
  go.mod says `go 1.26.x` and the fix list has both a `1.26.y` and a
  `1.27.y` entry, prefer staying on the `1.26` line unless only `1.27.y` is
  listed): `go mod edit -go=<version>`. You don't need that toolchain
  installed locally — `go build`/`go test` auto-download it
  (`GOTOOLCHAIN=auto` is Go's default) the first time they run against the
  bumped `go.mod`.
- **A module alongside `"stdlib"` for the same finding** (e.g.
  `golang.org/x/net` next to `stdlib` for an HTTP/2 finding) — bump
  whichever of the two this repo actually has in `go.sum`; if it's not a
  direct or indirect dependency today, the toolchain bump alone clears it
  and the module entry doesn't apply.

Make all the version bumps for every fixable finding before re-verifying —
don't re-run the full check after each one.

## 4. Verify

```sh
go build ./... && go vet ./... && make test && make lint && gofmt -l .
govulncheck ./...
```

Re-run `govulncheck ./...` (plain, not `-json`) and compare its "affected"
list against what you started with:

- **Every finding gone** — proceed to step 5.
- **`make check`-equivalent fails** (build/vet/test/lint broken by the
  bump) — do not open a PR. Comment the failure on the issue, label it
  `blocked`, and stop. Do not work around a test or loosen anything to make
  it pass; a breaking bump is a human decision.
- **Some findings remain** (no fix published upstream yet for one of them)
  — proceed to step 5 anyway for whatever you did fix, but say explicitly in
  the PR and the issue comment which CVE IDs are still open and why
  (no fix available at the versions you could reach).

## 5. Commit and open the PR

```sh
git add go.mod go.sum
git commit -m "chore: fix govulncheck findings from issue #<N>"
git push -u origin "$branch"
GITHUB_TOKEN= gh pr create --base main --head "$branch" \
  --title "chore: fix govulncheck findings (#<N>)" \
  --body "Closes #<N>

## Summary
<which CVE IDs were fixed, and how — module bump vs. Go toolchain bump,
with before/after versions>

## Test plan
- [x] \`make check\`
- [x] \`govulncheck ./...\` re-run clean<, except: list any CVE IDs still open and why>
"
```

If anything is still open per step 4, also:

```sh
GITHUB_TOKEN= gh issue comment <N> --body "<remaining CVE IDs, with the reason no fix is available yet>"
GITHUB_TOKEN= gh issue edit <N> --remove-label vuln-in-progress --add-label blocked
```

Otherwise:

```sh
GITHUB_TOKEN= gh issue edit <N> --remove-label vuln-in-progress --add-label in-review
```

Stop either way. A human reviews the PR normally; if the issue is `blocked`
on a remaining CVE, re-running this agent later (after an upstream fix ships)
needs the `vulnerability` label re-added by a human, since removing it here
is what stops the loop from retrying a fix that isn't available yet.

## Report

The issue number, PR number (if opened), which CVE IDs were fixed and how,
which remain open and why, and the full `make check`-equivalent output.
