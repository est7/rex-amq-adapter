# Round 2 verification by claude-04 (codex-01's SEC1 and SCAL1)

I re-derived both from source at f86e7a1 and re-ran codex-01's repro unchanged:
`cd /tmp/rex-audit-20261112/probe-codex-01 && REX_AMQ_ADAPTER_HOME=…/probe-codex-01/state go run -mod=mod .`.
I read the repro (main.go) before running it. It uses temp dirs under the probe directory, a fake
Terminal, and git with `GIT_CONFIG_GLOBAL=/dev/null`, and touches no real state. Output:

```
TRUST: copied pointer denied; symlink pointer settings_nonempty=true registered_in_git=false store_unchanged=true
CAP: adoption_after_layout=false action=refused live_records=6 pending=0 launched=0
CAP: adoption_after_layout=true action=start live_records=6 pending=1 launched=1
```

## SEC1: CONFIRMED (Medium)

The back-pointer check compares the `.git` *file*, not the directory, and `os.Stat` follows symlinks, so
a directory whose `.git` links to a registered worktree's `.git` passes.

Walk (internal/trust/trust.go):
- `MainCheckout(dir)` reads `dir/.git` with `os.ReadFile` (:133), which follows a symlink. So the gitdir,
  commondir and HEAD checks (:142-172) all validate the *registered* worktree's metadata.
- The guard meant to stop a foreign pointer, "The registration must point back at this very .git file"
  (:173-174), compares `os.Stat(registered)` with `os.Stat(dir/.git)` (:184-188). Both follow the link,
  so `SameFile` is true.
- The control case (a *copied* pointer, a new inode) is correctly denied. A hard link would pass for the
  same reason (same inode); I did not execute that variant.
- `Derive` then finds `dir` untrusted but `main` trusted (:70-76), and appends the unrelated `dir` to
  `trustedWorkspaces` (:78).

Automatic callers, which make this reachable without the user typing `trust`:
- app/summon.go:353 (`launch` derives trust for every started or resumed agent's cwd);
- cmd/rex-amq-adapter/main.go:567 (`tab` derives trust for `-c`).

Why Medium and not lower: the guarded asset is Antigravity's folder-trust dialog, which exists to stop
an untrusted directory's project config (such as `.agents/hooks.json`) from running. The precondition
is a directory whose `.git` is a symlink or hard link to a registered worktree's `.git`. Git itself
will not check out a path named `.git`. An extracted archive or an agent-written directory can carry
one, but the attacker must also know an absolute path of a registered worktree on this machine.

Fix as codex-01 sketches: compare `filepath.Dir(registered)` with `dir` as directories (`os.Stat` on
both, `SameFile`), and reject `dir/.git` when `os.Lstat` reports a symlink or a link count above 1.
Keep directory-alias (case and symlinked-directory) spellings of the *same* worktree working.

## SCAL1: DOWNGRADE to Low

The mechanism is real, but the interleaving it needs is far narrower than the repro's injection point
suggests, and the cap it breaks is a soft UX limit that the documented launch paths skip anyway.

Confirmed mechanism:
- `Summon` snapshots the layout once (app/summon.go:82, `FindSession` then `loadSession` → `ListBlocks`,
  app/discovery.go:43-46).
- `reserve` re-reads records under the lifecycle lock but counts live ones through that snapshot's
  `s.holds` (app/summon.go:229-233). `holds` needs the block to be in `windowOf` (app/discovery.go:71-73).
- A record whose block was not in the snapshot is neither live-counted nor pending once adoption has
  consumed its token (adopt.go:206, then the PutRecord at :220).

Why the real window is narrow (traced):
- The in-flight agent's block is created by the first summoner's `NewSplit` (app/summon.go:369/376)
  *before* the agent boots. So any second snapshot taken after that split contains the block and counts
  it once it adopts.
- To miss it, the second summoner's `ListBlocks` must fall between the first summoner's `reserve` and
  its `NewSplit`, a few Rex RPCs wide. Then the new agent must boot, run its first hook, and finish
  `Adopt` before the second summoner gets the lifecycle lock in `reserve`.
- That last step is possible only when the lock is contended. `Adopt` holds it for its whole body
  (app/adopt.go:40-44), across Rex calls and `amq.Ensure`, so a burst of concurrent adoptions can delay
  `reserve` by about the length of an agent's boot.
- The repro forces the adoption inside `InspectSession` (probe main.go, `afterLayout`), which skips that
  ordering constraint.

Consequence when it does hit: one agent over a cap whose own rationale is a UX bound, "six panes is what
one person can still follow" (core/summon.go:14-17). It is not a security or resource limit. That cap is
already skipped by every non-summon launch path (`tab`, raw `rex split` / `rex run` / `rex new`: my
ARCH-2). The over-cap agent is still subject to the depth, cycle and grant guards.

Fix (cheap, keep it): have `reserve` call `loadSession` under the lifecycle lock (or count `Live()`
records with `recorded(rec)` as a fallback), plus the barrier regression codex-01 proposes.
