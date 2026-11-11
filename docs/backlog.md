# Backlog

Findings that are known but not scheduled. They come from the
tech-debt and architecture pass behind plans 001–009 (shipped in v0.11.4)
and from the two watchdog audits before it. File and line references were
taken at `9a12346` unless stated otherwise; check them against the code
before acting on one.

## Unplanned — waiting on the lifespan decision

The adapter's expected lifespan is still open (agent-post is meant to
replace it). That decides whether the M/L refactors (U1, U2, U4) are worth
doing.

| ID | Finding | Effort | Notes |
|----|---------|--------|-------|
| U1 | **Harness static facts have no single owner.** About 60 per-kind branches across 12 files. `core.Dirs` (`internal/core/harness.go`, plan 001) is the start of a home for them. The rest: launch/resume argv (`app/summon.go:362-391`), session env vars (`app/caller.go:20-28`), the hook event vocabulary (defined in both `install/install.go:104-155,667` and `app/hook.go:17-30` plus `hookEventNames`), the agy settings path (`install.go:57`, `trust.go:42`), and Claude's supervisor roster parsed twice (`app/claudejob.go:40,162`). | M–L | Static facts only. Behavioral identity checks (`hook.go:56/62/70`, `adopt.go:56-72`, `lease.go:108`) stay as branches in the flow; see Rejected. |
| U2 | **AMQ has no seam.** `Terminal` fakes Rex, but amq is called as package functions, so most app test files need the real `amq` binary (`lifecycle_test.go:18-23`, `realAmqEnv`). On a machine without it, about 75% of app tests **skip** while `-short` reports green. Release CI runs only `go vet` (`.github/workflows/release.yml:24`). | M | Do this first among U2/U4: it makes U4 cheap to test. A pure call-site refactor. |
| U3 | **Remaining core policy tests.** `TinyPane`, `GuardSummon` and `ErrorRecipient` still have no test in `internal/core`; `ErrorRecipient`'s table test lives in `app/health_test.go:520`, and `Assess` is tested only from `internal/transcript`. | S | Can go any time. |
| U4 | **Pure decisions inside app, interleaved with I/O.** Adopt's re-adoption rule (`app/adopt.go:148-212`, where `Rex.Foreground` and the consuming `Store.TakeSummon` sit mid-block and must be hoisted with care) and summon target resolution and refusals (`app/summon.go:91-169`). | M ×2 | Depends on U2. Existing world tests stay unedited as the parity oracle; new core table tests are additive. |
| U5 | Minor duplication. The inject argv layout is positional in 3 places (`amq/wake.go:47`, `amq/lifecycle.go:149`, `cmd/rex-amq-adapter/main.go:245`). Trust derivation differs between `cmd/rex-amq-adapter/main.go:549` and `app/summon.go:343`. summon's deliver-after-adopt step lives untested in `main.go:600-620`. The three log writers (`summonLog`, `wdLog`, `blockedLog`) are hand-rolled separately. | S each | Low change pressure. If the log writers are merged, keep plan 002's rule: an audit log never decides the outcome of the action it records. |
| U6 | Dead code. `core.SummonLine` has no references. `core.LastDelivery` and `core.DeliveryStatus` are used only by tests (DeliveryStatus is a parity copy of `DoorbellSummary`). | S | Deleting DeliveryStatus means rewriting `TestDoorbellSummaryIsDeliveryStatus` with literal expectations. |

## Open findings from the watchdog audits

From agy-65's state-machine audit and its signoff rounds on the v0.11.4
watchdog refactor. None was fixed there; agy-65 verified each claim marked
"verified", the rest are its claims only.

- **Codex hung-tool reports can be dropped** (verified). The hung-tool
  dedup in `app/health.go` compares `Row.ID`, which Codex rows take from
  `"ordinal"`; a row without it has an empty id, which equals a fresh mark's
  empty `LastHungID`, so the report is never sent. Key the dedup on
  `Row.ToolID` instead.
- **`ErrorRecipient` remembers only the first skipped family** (verified).
  `core/health.go` keeps a single `skippedKind`, so step 5 can route a
  turn error to another family that is also flagged. Track every skipped
  kind.
- **An empty session id reads as `staleLease`** (verified, rarely
  reachable). `app/lease.go` returns `foreign` only when both session ids
  are non-empty; a hook with none falls to `staleLease`, and
  `reportTurnError` / `clearTurnError` then act in the pane owner's name.
  Claude Code and Codex hooks always carry a session id.
- **HealthMark keying and lock scope** (agy-65's claims, not verified):
  `LastHungID`, `LastStallAt` and the turn-error fields are keyed by block,
  so a resumed or moved agent can lose or inherit them; `LockHealth` is held
  across Rex, amq and `osascript` calls that stop hooks wait on.
- **Pi's session directory override.** Pi 0.84.2 also honors
  `PI_CODING_AGENT_SESSION_DIR` (`dist/main.js:551`), which moves sessions
  out of `PI_CODING_AGENT_DIR`. `core.Dirs` does not read it.
- Also from that audit, not pursued: two orchestrators in one session share
  a block's episode without a lock; a stale deferral run from long ago lets
  new mail skip the 30 s wait; a 0x0 size counts as large enough; the
  watchdog looks read the viewer block's status, not a Claude background
  job's; Codex may re-report the previous turn's error when the new turn
  wrote no row.

## Considered and rejected

- **Split `app.Env` (≈131 methods) into packages by subsystem.** The
  clusters share internals: the watchdog calls `Inject`, `checkBinding`,
  `CollectSession` and `seedSentCursor`, and health calls `recordFit` and
  `adoptUnlessBackingOff`. A split would export those helpers and add no
  isolation. Test cost comes from missing seams (U2/U4), not from `Env`'s
  size.
- **A `Kind` interface with behavior methods (a strategy per harness).** The
  per-kind branches in `hook.go`, `adopt.go` and `lease.go` are identity
  proofs (Claude background jobs, Codex shared-daemon ancestry, agy
  subagents) interleaved with the adoption flow. An interface over them would
  be a shallow pass-through. Consolidate static facts only (U1).
- **Move `amq.DecideWake` and `store.PendingSummonFor` into core.** They are
  pure and tested where they live.
- **Extract the watchdog's per-look decision into core.** After `9e215f1`
  the switch is short and typed over `core.Placement`, and the state tests
  pin each transition.
- **`Depart` ignores kind and empty session ids.** Every payload that
  reaches `Depart` carries a non-empty session id, and a nested foreign
  CLI's `SessionEnd` carries its own id, which `lifecycle.go:112` already
  ignores. An empty id comes only from a hook run by hand, so there is no
  reachable failure.
- **`together` ignores the server; `runTab` uses `REX_SERVER`.** Rex session
  ids are ULIDs, so they cannot collide across servers in practice. The
  `runTab` server mismatch arises only for a record on another server and is
  part of U5.
- **Background-job status routing for the watchdog looks; skipping
  `CaptureVT` while working.** Niche, with no observed misreport, and
  checking it needs the real agent view. The `CaptureVT` skip is a
  micro-optimization.
- **`sendAs` (advance the sent cursor only when nothing is pending).**
  Superseded by plan 008's system-mail label: it still bounced system mail
  that shared a batch with the agent's own mail, and it held two file locks
  across a subprocess.

## Out-of-scope observations

- `app/claudejob.go:162-164` (`jobNames`) reads `daemon/roster.json` before
  checking `e.ClaudeDir == ""`, so an empty dir reads a path relative to the
  cwd. Harmless today.
- Not audited: correctness, security, performance, dependencies, DX and docs
  beyond what the architecture pass hit; `install.go` past its agent table
  (about 700 lines); `store` and `rex` internals.
