---
topic: review
summary: Run a review in one of two tiers: one seat, or a council with scores
---
# Review tiers

Review runs in one of two tiers, `solo-loop` and `council`. In both, the author
makes the smallest change that answers a finding, pushes back with evidence,
and fights scope creep: a finding outside the brief is declined in one line and
listed as a follow-up.

## One seat, the default (`solo-loop`)

One reviewer of another model family:

```bash
rex-amq-adapter agent summon reviewer --subject "review S1" --body @brief.md --grant grant.md --kind review_request
```

- **Budget:** 3 rounds; round 1 is full, later rounds check only changed code
  and the open finding ids. Extend to 5 only for security, permissions, auth,
  data loss, or state/concurrency while a `blocker` or `should` stays open with
  evidence, or when the user asks.
- **Stop early** when a round returns only nits; declined nits and comment,
  doc, or format-only fixes need no new round.
- **Keep a ledger** (id, severity, outcome). Done is `APPROVE` on the current
  behavior diff with no `blocker` or `should` open; the end of the budget is
  not approval.

## Council

When the user says "council" (a score after it, "council 9", is the bar).
Before it, fix known issues and run the checks, so the seats read the updated
change. Then:

```bash
rex-amq-adapter agent summon reviewer --council --cwd <worktree> --grant <file> --kind review_request --subject "<task>" --body "<the request>"
```

It puts the request in front of one seat per harness of
`[role.reviewer].harness` (codex, agy, claude), each asked for `SCORE: <n>/10`:
a live seat in that worktree gets the mail, the rest start (and without `--cwd`,
a departed seat resumes). Every seat that needs a launch is checked first; when
one is not installed, fails its preflight, or would pass the session cap, the
summon is refused naming each and nothing is mailed or launched. Report the
missing seat; never substitute another harness. `--council` takes a read-only
role only, never a handle or `--harness` (exit 2). The JSON lists `seats`, each
with its harness, action, handle, and startup.

A design Epic's lead architect seats its own council
(`agent summon architect --council --cwd <its directory>`): it holds its
harness's seat itself (`action: self`), so the council is the lead and one
architect on each other harness.

Each seat replies `VERDICT: APPROVE | CHANGES`, `SCORE: <n>/10` when asked, and
`S1 [blocker|should|nit] <file>:<line> <claim>. Fix: <suggestion>`, and builds
or tests only in a temporary detached worktree of its own.

Act on each result as it arrives, but edit nothing in the checkout while a seat
still reads it; a seat the user marks non-blocking is not waited for, and a late
result from it is feedback whose score is not recorded. Each full pass is a
round; a fresh review after fixes (the same command: live seats get it) starts
the next. Continue until every seat is satisfied, or every seat reaches the
user's score when they named one; there is no other round cap and no default
score. Record the scores in the PR description (edit it; do not comment), or in
the final reply when there is no PR, before the model and harness line:

```markdown
## Council review

Rounds: 2

| Seat | Round 1 | Round 2 |
| --- | --- | --- |
| codex (gpt-6-astra) | 7/10 | 9/10 |
| agy (gemini-4-carbon-high) | 8/10 | 10/10 |
| claude (opus) | 7/10 | 9/10 |
```

A seat that gives no score is `no score`; never invent one.

## Stuck, in either tier

An item stays disputed after two replies, or the one-seat budget ends: stop,
show the user both sides in 2-3 lines each, and let the user decide.
