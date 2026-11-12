# rex-amq-adapter repository audit (2026-11-12)

Target: `main` at `f86e7a195b3442c381434220f737b8d0561e61f5` (v0.12.0). Read-only audit; no product
code, test, CI file or existing doc was changed. Every finding carries an evidence level (located,
traced, executed, reproduced). Every finding first raised as High or Critical was re-derived by an
agent of another model family, and most of them were downgraded.

Auditors: codex-01 (Codex; security, performance, scalability), claude-04 (Claude; architecture,
maintainability, documentation), codex-02 (Codex; testing, automation, accessibility, sole gate
runner), agy-01 (Antigravity/Gemini; all nine dimensions, independent and blind to the others'
round-1 results). Orchestrator: claude-03 (Claude).

## Summary

The codebase is healthy for its size. Every gate passes (build, vet, short tests, race tests, the
real-Rex e2e suite, govulncheck). `internal/core` is I/O-free, and the import graph is acyclic. State
files are private and serialized by locks. Subprocess argv is built as arrays; the only shell is the quoted `bin/inject` wrapper.

**One High finding survived verification:** the narrator's documented channel for relaying a user
directive to its orchestrator cannot work, because no code ever produces the control operation the
gateway requires (ARCH-3). No Critical finding survived. agy-01's Critical managed-mail finding was
executed and is real, but it stays inside the same-UNIX-user trust the design already grants raw
`amq`, so it is filed as Medium (SEC-2).

The Medium findings cluster in three places:

1. **The paths agents are taught are not the paths the design protects.** Every notice, doorbell,
   guide and role prompt teaches bare `amq`, which skips the managed gateway's drain-time gate and
   send provenance (ARCH-1). The managed verbs themselves can be steered by trailing flags (SEC-2).
   Agents started outside `summon` skip its guards, and a failed record write resets lineage (ARCH-2).
2. **Automation stops at the release tag.** No CI runs on push or PR, and release CI silently skips
   every suite that needs real Rex or amq (AUTO-1, TEST-1). A dev build's `who` rewrites the shared
   doorbell injector (AUTO-5).
3. **The agent-facing surface is too thin to plan a crew.** An agent cannot read a crew's or role's
   prompt without the source. `--help` and `--json` behave inconsistently, and one `--help` writes
   state (DOC-1, A11Y-2, AUTO-2). This audit's own run hit each of these.

## Findings

Ranked by severity, then by reach. "Source" is the raising agent; full write-ups with every probe
command and output are in `docs/audit/2026-11-12-evidence/` (`sec-perf-scal.md` = codex-01,
`arch-maint-doc.md` = claude-04, `test-auto-a11y.md` = codex-02, `agy-all.md` = agy-01,
`verify*.md`, `gates.md`, `crew-friction.md`). Each auditor numbered its findings independently; the
IDs below are this report's, and the Source column gives the original ID.

| # | ID | Severity | Dimension | Title | Evidence | Source | Verified |
|---|---|---|---|---|---|---|---|
| 1 | ARCH-3 | High | Architecture | Narrator-to-orchestrator control relay has no producer; every directive is refused | executed | agy-01 ARCH-2; claude-04 | codex-02: CONFIRMED High |
| 2 | SEC-2 | Medium | Security | Managed `send`/`reply` let trailing flags override the adapter's `--me/--to/--context` after the gateway check | executed | agy-01 SEC-1 (Critical) | codex-01: DOWNGRADE to Medium |
| 3 | SEC-1 | Medium | Security | `trust` extends repository trust to an unrelated directory whose `.git` symlinks to a registered pointer | executed | codex-01 SEC1 | claude-04: CONFIRMED |
| 4 | ARCH-1 | Medium | Architecture | Every instruction surface teaches bare `amq`, bypassing managed drain, send provenance and the narrator view | executed | claude-04 ARCH-1; agy-01 ARCH-1 (High) | codex-01: CONFIRMED; codex-02: DOWNGRADE agy's High to Medium |
| 5 | ARCH-2 | Medium | Architecture | Summon token consumed before the record is written; a failed write re-adopts at depth 0 | executed | claude-04 ARCH-2 | codex-01: CONFIRMED (narrowed) |
| 6 | SEC-3 | Medium | Security | `summon` passes a dash-leading brief to Claude/Codex/Pi without `--`, so it parses as an option | executed | agy-01 SEC-2 (High) | codex-01: DOWNGRADE to Medium, narrowed |
| 7 | AUTO-1 | Medium | Automation | No CI on push or PR; first automated check runs on a release tag | located | codex-02 AUTO1; agy-01 TEST-1 | - |
| 8 | TEST-1 | Medium | Testing | Release CI silently skips the real Rex/amq suites (>=117 of 271 app tests) | traced | codex-02 TEST1; claude-04 | - |
| 9 | AUTO-5 | Medium | Automation | `who`, `stop`, `collect` and hooks rewrite the shared `bin/inject` to the running binary | traced | agy-01 AUTO-1 | - |
| 10 | DOC-1 | Medium | Documentation | The agent guide cannot plan a crew for a goal without reading source | executed | claude-04 DOC-1; est9; orchestrator | - |
| 11 | MAINT-1 | Medium | Maintainability | Adding an agent kind per the doc leaves `summon` silently launching Claude for it | traced | claude-04 MAINT-1; agy-01 DOC-1 | - |
| 12 | DOC-2 | Medium | Documentation | design.md lacks the summon guard model; README and guide omit the managed, `link` and `view` commands | located | claude-04 DOC-2; agy-01 DOC-1 | - |
| 13 | SCAL-2 | Medium | Scalability | `provenance/`, `quarantine/`, per-block state files, logs and amq `config.json` grow without bound | traced | agy-01 SCAL-1 | - |
| 14 | PERF-2 | Medium | Performance | `events` rescans every mailbox and ledger each second and calls Rex serially every 2 s | traced | agy-01 PERF-1 | - |
| 15 | MAINT-2 | Medium | Maintainability | The backlog's reason for deferring seams is stale while `internal/app` grew 39% | executed | claude-04 MAINT-2 | - |
| 16 | MAINT-3 | Medium | Maintainability | Package-global `app.Diagnostics`; one 10 s budget shared by every watcher phase | located | agy-01 MAINT-1 | - |
| 17 | SEC-4 | Medium | Security | Hook-supplied session IDs and transcript paths reach `filepath.Glob` and file reads unvalidated | traced | agy-01 SEC-3 | - |
| 18 | SCAL-1 | Low | Scalability | An adoption between layout snapshot and `reserve` lets a summon exceed the 6-agent cap | executed | codex-01 SCAL1 | claude-04: DOWNGRADE to Low |
| 19 | A11Y-1 | Low | Accessibility | Claude input boxes whose title fills a narrow top border are classified missing | executed | codex-02 A11Y1; agy-01 A11Y-1 (High) | codex-01: DOWNGRADE to Low (twice) |
| 20 | PERF-1 | Low | Performance | Every PostTool hook decodes all records and lists the whole outbox, even with no new mail | executed | codex-01 PERF1; agy-01 PERF-1 | - |
| 21 | A11Y-2 | Low | Accessibility | `title --help` writes `--help` as the task; `crews --help` prints the table; `send` blocks on a TTY | reproduced | codex-02 A11Y2; agy-01 AUTO-2 | - |
| 22 | AUTO-2 | Low | Automation | No `who --json`; `crews --json` silently prints text and treats the flag as a directory | reproduced | codex-02 AUTO2; claude-04 AUTO-2 | - |
| 23 | AUTO-3 | Low | Automation | No amq capability check and no state-format stamp, though README requires both by hand | located | claude-04 AUTO-1 | - |
| 24 | A11Y-3 | Low | Accessibility | A multi-agent tab keeps the first agent's pane label as its name | executed | orchestrator | - |
| 25 | AUTO-4 | Low | Automation | `await --timeout 60` fails; the flag needs a Go duration the guide does not show | reproduced | orchestrator | - |

### Fix plan

One row per finding: the change, and what it touches beyond its own file. Fixes are not scheduled;
which to take, and in what order, is the maintainer's call.

| ID | Fix sketch | Blast radius |
|---|---|---|
| ARCH-3 | Managed `send --control <request-id>`, narrator role only, producing `OpControl`; document in `narrator.md` and the guide | managed `send` flags, narrator prompt, guide; provenance schema unchanged |
| SEC-2 | Parse each managed verb with its own flag set; reject routing and unknown flags; build child argv from parsed fields | `send/reply/read/drain/thread/trace/dlq/list` flag acceptance (callers passing raw amq flags get exit 2) |
| SEC-1 | Compare git's registered back-pointer directory with the candidate; `Lstat`-reject symlinked or hard-linked `.git` | `trust` output and agy `--settings` via `summon`/`tab`; no persisted state |
| ARCH-1 | After SEC-2, switch doorbell, notices, summon text, guide and role prompts to `rex-amq-adapter drain/send/reply`; bounce a quarantined cross-Epic send to its sender | model-facing text and the core/e2e tests asserting it; CLI unchanged |
| ARCH-2 | Adopt: read token, commit record, then delete token; route `tab` through `reserve`, or tell depth >= 1 agents to start peers only via `summon` | adoption order in `internal/app/adopt.go`; `tab` may start refusing; guide text |
| SEC-3 | `--` before the prompt for harnesses that honor it, a leading-dash guard otherwise; `--grant` must be a regular file | launch argv per harness in `startCommand`/`resumeCommand` |
| AUTO-1 | Push/PR workflow: build, vet, `-short`, `-race`, govulncheck | new workflow file; no runtime change |
| TEST-1 | macOS integration job with pinned Rex and amq that fails on a required skip; CLI flag-dispatch unit tests | CI provisioning and supported tool versions |
| AUTO-5 | Pin the installed binary at `setup`; `prepareInjector` refuses to repoint `bin/inject` to a temp or unpinned binary | `who`, hooks, `setup`; the `bin/inject` contract amq wake relies on |
| DOC-1 | `crews show <crew\|role>`, a purpose line per crew and role, a "Plan a crew" guide section | additive CLI subcommand; guide text |
| MAINT-1 | Unlisted kind is an error in `startCommand`/`resumeCommand`; add the launch step to the kind checklist | summon refusal text for unknown kinds; doc |
| DOC-2 | design.md summon section, module and path lists, section-4 subheadings; README and guide command tables | docs only |
| SCAL-2 | `collect` expires provenance, quarantine, per-block files and logs past a retention window; retire dead handles from amq config | `collect` behavior; nothing live or within the 7-day reservation is removed |
| PERF-2 | Stat before re-reading, tail-read inject logs, bounded parallel Rex calls in `events` | `events` internals; NDJSON schema unchanged |
| MAINT-2 | Decide the adapter's lifespan in backlog.md; if it continues, land the `amq` interface (U2) | process and docs; U2 is an internal refactor |
| MAINT-3 | Move the diagnostics writer onto `Env`; per-phase budgets in `watchOnce` | internal to `internal/app` |
| SEC-4 | Validate hook ids against a strict charset; confine transcript paths to the harness root; harden `fileKey` | hook normalization; existing ids on disk stay valid |
| SCAL-1 | Reload the session layout under the lifecycle lock inside `reserve` | summon admission at the cap |
| A11Y-1 | Recognize a title-first Claude top border by its paired prompt and bottom rule; keep the transcript-line rejection | doorbell readiness for Claude and agy panes |
| PERF-1 | Rebuildable block-to-handle index; skip an unchanged outbox by directory stamp | store lookup and sent-cursor bookkeeping; index must stay derivable |
| A11Y-2 | Handle `-h/--help` before resolving the caller or writing state; exit 0; no implicit stdin read from a TTY | help and exit codes of `title`, `crews`, `join`, `events`, `view`, `send` |
| AUTO-2 | `who --json`, `crews --json`; reject unknown flags; alias `--version` | additive output schemas; invalid input now errors |
| AUTO-3 | amq, Rex and state rows in `status`; a `state-version` file with one in-band notice on mismatch | `status --json` items; one new state file |
| A11Y-3 | When a follower tab gains a second live agent, show the lead's task alone or the placeholder | tab naming rules in `internal/core/tab.go` |
| AUTO-4 | Accept bare seconds in `await --timeout`, or show `60s` in the guide | `await` flag parsing or guide text |

Severity of unverified Medium findings is the raiser's. SEC-4 and SCAL-2 sit at the edge of the
same-UNIX-user trust the design grants (`docs/design.md:1195-1197`) and may rate Low once checked.

## Security

**SEC-2 (Medium; executed by codex-01; raised as Critical by agy-01).** `takeFlag`
(`internal/app/managed.go:442-465`) treats any standalone `--` as end of options while scanning for a
flag, without knowing other flags' value arity (`:445`). Native amq parses `--` as the value of
`--subject` and keeps parsing. The gateway check runs first (`managed.go:42`). The caller's remaining
arguments are then appended after the adapter's own `--me/--to/--context/--json`
(`internal/app/gateway.go:191`), and Go's `flag` lets the later value win. codex-01 ran it on real
amq roots. A send that passed the gateway as a same-Epic message was delivered cross-Epic under
another handle. With `--json=false` it wrote no provenance (`gateway.go:197`). The receiver's managed
drain admitted it, because `judge` fell back to the claimed sender (`gateway.go:47-51`). `reply` was
steered the same way. Both calls returned an error after delivering, so the caller is also misled.
Two attempts to make `read` leak a foreign body failed, so the claim that every inbound verb leaks is
not established. *Why Medium, not Critical:* any same-user process can already run raw `amq --me
<other>` or read mailbox files (`docs/design.md:1195-1197`). This defeats the supported path's policy
but grants no new host capability.
*Fix:* parse each managed verb once with a per-verb flag set, reject routing flags and unknowns, and
build the child argv from parsed fields. Moving the `--` stop alone keeps the differential.
*Blast radius:* `rex-amq-adapter send|reply|read|drain|thread|trace|dlq|list` flag acceptance.

**SEC-1 (Medium; executed by codex-01, verified by claude-04).** `internal/trust/trust.go:184-188`
compares the registered worktree's `.git` and the candidate's `.git` with `os.Stat` + `os.SameFile`.
`Stat` follows symlinks, so a directory whose `.git` symlinks to a registered pointer passes, and
`Derive` adds that unrelated directory to `trustedWorkspaces` (`trust.go:78`). It is reached
automatically from an agy `summon` (`internal/app/summon.go:353`) and from `tab`
(`cmd/rex-amq-adapter/main.go:567`). A real-git probe denies a copied pointer but emits trust
settings for the symlinked one. A hard link has the same inode problem (reasoned). It needs same-user
file creation, so it widens trust inheritance rather than opening a remote hole.
*Fix:* compare the directory of git's registered back-pointer with the candidate itself, and
`Lstat`-reject a symlinked or `nlink>1` `.git`. Add the regression beside
`TestCopiedGitPointerDerivesNothing` (`internal/trust/trust_test.go:105`).

**SEC-3 (Medium after verification; executed).** `startCommand` passes the brief as the last
argument to Codex, Pi and Claude without `--` (`internal/app/summon.go:400-410`, brief built at
`:324`). With a fake terminal, codex-01 captured a dash-leading brief in that slot. With the installed
Codex parser, a brief starting `--help=...` exits 2 as an option and the agent never starts a turn.
The other claims in agy's SEC-2 are not defects. agy's `--dangerously-skip-permissions` is a
documented policy (`README.md:95-96`, `skill.md:232-234`), and `--asked` or `--grant` is a documented
attestation. A directory accepted as `--grant` is a smaller validation gap.
*Fix:* put `--` before the prompt where the harness honors it; otherwise guard a leading dash. Require
`--grant` to name a regular file.

**SEC-4 (Medium per raiser, traced, unverified).** `HookInput` (`internal/app/env.go:93-135`) takes
`session_id` and `transcript_path` from hook stdin without validation. The session id is
interpolated into `filepath.Glob` (`internal/transcript/transcript.go:58-75`). The transcript path
reaches `ReadPath` (`internal/app/health.go:131-133`), which can put the tail of an arbitrary file
into an error notice. `store.fileKey` (`internal/store/store.go:156`) leaves `..` intact. Hook stdin
comes from the harness the user runs, so the realistic risk is a malformed value, not an attacker.
*Fix:* validate ids against `^[A-Za-z0-9._-]+$` (rejecting `.` and `..`), and confine transcript
paths under the harness's transcript root.

ARCH-2 (summon lineage reset) is also an authorization weakness; it is filed under Architecture
because the defect is where the guard sits.

Strengths: doorbells carry only fixed text, a count and a validated handle
(`internal/core/inject.go:188`). Rex and amq are called with argv arrays (`internal/rex/rex.go:76`,
`internal/amq/wake.go:315`). State files are created 0600 and directories 0700
(`internal/fsutil/fsutil.go:17,49`). Incoming mail with a malformed header fails closed
(`internal/app/gateway.go:89`). govulncheck reports no known vulnerabilities.

Documented non-goals not re-filed: same-user agents can read each other's mailboxes and send raw
`amq` (`docs/design.md:1196`, `internal/core/gateway.go:12`); `--asked` is self-attested by design.

## Performance

**PERF-2 (Medium per raiser, traced, unverified).** `events` ticks every second
(`internal/app/events.go:137`). Even when idle, it re-reads each handle's ledger and
`inject/<h>.jsonl` (up to 1 MB) and sorts every historical inbox and sent id to keep the newest 50
(`internal/app/observe.go:208-285`). Every 2 s it runs `rex block call ... program_status` serially
per agent (`observe.go:171-194`), which can overrun the tick as agents grow (not measured).
*Fix:* stat before re-reading, tail-read the inject log, bound concurrency of the Rex calls.

**PERF-1 (Low, executed).** Each announcing hook calls `Store.Record` (`internal/app/hook.go:200`),
which decodes every handle's JSON (`internal/store/store.go:165,235`). The PostTool hook's
`checkableSent` lists and sorts the whole outbox before applying its cursor (`hook.go:227`,
`internal/amq/lifecycle.go:257,279`). On the real `Env.Hook` (5 interleaved runs, warm cache), 6
records took 0.16 ms, 1000 records 11.8 ms, and 20,000 already-checked sent files 8.2 ms. That is
small against the hook budget: it is scaling debt, not a measured slowdown.
*Fix:* a rebuildable block-to-handle index, and skip an unchanged outbox by its directory stamp.

## Scalability

**SCAL-2 (Medium per raiser, traced, unverified).** `provenance/` and `quarantine/` sit under
`extensions/rex-amq-adapter` precisely so `amq cleanup` leaves them alone
(`internal/store/gateway.go:19-20`), yet nothing expires them. `DeleteRecord` leaves `quarantine/<h>`
behind (`internal/store/store.go:200-207`). Per-block and per-window files (`tabs/`, `watchdog/`,
`health/`, `turn-error/`, `adopt-failed/`) are never removed when panes close. `logs/*.log` never
rotate. `EnsureMailbox` appends every new handle to amq's `meta/config.json` forever
(`internal/amq/lifecycle.go:157-186`), which `takenHandles` scans linearly
(`internal/app/adopt.go:409-435`).
*Fix:* extend `collect` to expire these past a retention window, and cap log sizes the way
`inject/<h>.jsonl` is already trimmed.

**SCAL-1 (Low after verification, executed).** `Summon` snapshots the layout
(`internal/app/summon.go:82`) and later counts live records against that stale snapshot in `reserve`
(`summon.go:231`, `internal/app/discovery.go:72`). An agent adopted in between is in neither count, so
a seventh agent can start. claude-04 downgraded it. A miss needs a precise ordering under
lifecycle-lock contention (`internal/app/adopt.go:40-44`), and the cap is a UX limit that `tab` and
`rex split` already skip.
*Fix:* reload the layout under the lock inside `reserve`.

Not measured: live process counts, six real agents starting at once, many sessions, weeks of
retained mail.

## Architecture

**ARCH-3 (High; executed by codex-02 and by claude-04; raised by agy-01).** `core.CanExchange`
allows narrator-to-orchestrator mail only with `OpControl` and a non-empty `ControlRequestID`
(`internal/core/gateway.go:147-151`). No code ever constructs that operation. Every outgoing message
is `OpSend` or `OpReply` (`internal/app/managed.go:91-109`, `internal/app/gateway.go:51,230,237`,
`internal/app/narrator.go:125`). So the relay `internal/crew/builtin/roles/narrator.md:7` instructs,
which carries a user's change of requirements, stop or decision to the orchestrator, is always
refused (`narrator_sends_orchestrator_control_only`). Fixing the instructions (ARCH-1) does not help.
codex-02 drove both the managed and raw paths and both refused a valid directive; the pure-policy
positive control allows it. claude-04 hit the same defect independently, with a failing test
(`TestManagedMailNarratorControlReachesOnlyTheOrchestrator`) on f86e7a1.
*Fix:* a control-send surface (for example `rex-amq-adapter send --control <request-id>`), allowed
only for the narrator role, setting `Op{Kind: OpControl, ControlRequestID: id}`, and documented in
`narrator.md` and the guide. A candidate exists on the unmerged branch `fix/agent-guidance`.
*Blast radius:* managed `send` flags, narrator prompt, guide. The provenance schema already has the
fields (`internal/store/gateway.go:41-42,104-112`).

**ARCH-1 (Medium; executed by codex-01 and codex-02).** `docs/design.md:1178-1193` names the
gateway-checked path. Managed `send`/`reply` record provenance, and managed `drain` consumes only the
ids its gate admitted (`internal/app/managed.go:181-212`). Yet every surface teaches bare `amq`: the
doorbell (`internal/core/inject.go:189`), hook notices (`internal/core/announce.go:103,121`), summon
(`internal/app/summon.go:278,325`), the guide (`internal/guide/skill.md:82,123`) and the role prompts.
Executed in isolated amq roots, this has three consequences:
- bare drain delivered mail that arrived after the last gate pass, which managed drain quarantined;
- a bare send writes no provenance, so in the narrator's view that message is an
  `unverified_provenance` gap and `view read` refuses its body; managed, summon and system mail
  still contribute (`internal/app/view.go:382-391,419-439,534-543`);
- a cross-Epic bare send exits 0 but is quarantined on arrival, and the sender is never told
  (`internal/app/undelivered.go:193-206`).

The managed positive control filled the view correctly, so this is the instruction surface, not the
mechanism.
*Fix:* switch notices, guide and role prompts to `rex-amq-adapter drain|send|reply` (after SEC-2 is
fixed), or delete the managed verbs and say no drain-time gate is offered.

**ARCH-2 (Medium; traced by claude-04, executed by codex-01, narrowed).** `store.TakeSummon` deletes
the summon token (`internal/store/store.go:855`) at `internal/app/adopt.go:206`, before the record is
committed at `:220`. With a record-write failure injected, the reservation was consumed, and the
successful retry adopted the agent at depth 0 with no parent instead of depth 2.
*Fix:* read the token, commit the record, then delete the token.
*Policy gap in the same area, not a code defect:* `core.GuardSummon` has one call site
(`internal/app/summon.go:239`). `tab` (`cmd/rex-amq-adapter/main.go:525-579`) and the guide's
`rex split/run/new` recipes (`skill.md:215-221`) start agents with no cap, depth or grant check, at
depth 0 (`adopt.go:200-210`).

Strengths: `internal/core` has no `os`, `os/exec`, `net` or `time.Now()` in non-test code. The import
graph is acyclic with core at the bottom. Rex sits behind a narrow `Terminal` interface
(`internal/app/env.go:22-44`). Once its record is written, `Adopt` recovers from a crash because it writes `Unfinished=Adopting`
first (`adopt.go:212-268`); the step before that is not covered (ARCH-2). The coupling to amq's on-disk format is documented and schema-checked
(`internal/amq/mailbox.go:31`).

## Maintainability

**MAINT-1 (Medium, traced).** `docs/adding-an-agent-kind.md` section 4 (`:150-200`) never names
`startCommand`/`resumeCommand`, `DefaultHarness`, `TakesTitle`, trust derivation, or the install
supervisor and task files. `startCommand` ends in `return []string{"claude", brief}` for any unlisted
kind (`internal/app/summon.go:400-410`). A maintainer who follows the doc therefore gets
`summon --harness <new>` launching Claude Code under the new kind's label, and summon reports success.
Per-kind branch lines grew from 66 to 74 across 18 files since 9a12346.
*Fix:* return an error for an unlisted kind, add the launch step to the checklist, and longer term
build backlog U1's per-kind fact table with a test over every `Kind`.

**MAINT-2 (Medium, executed git measurements).** `docs/backlog.md:9-13,65-70` defers the `amq` seam
(U2) and the `Env` split, quoting about 131 methods. Since 9a12346, `internal/app` non-test code grew
from 5,621 to 7,795 lines and `Env` methods from 134 to 175. The gateway, managed mail, view and
narrator subsystems all landed inside `app`. `(Env).Adopt` is 232 lines with six interacting booleans
(`internal/app/adopt.go:39`).
*Fix:* make the lifespan call explicit. Either freeze the adapter to fixes, or land U2 now, which
also lets CI run the amq-dependent tests (TEST-1).

**MAINT-3 (Medium per raiser, located, unverified).** `app.Diagnostics` is a mutable package global
(`internal/app/lifecycle.go:21`) that `runHook` swaps for a log file it later closes
(`cmd/rex-amq-adapter/main.go:177`). `watchOnce` gives one 10 s budget to reconcile, watchdog and the
narrator refresh (`internal/app/orchestrator.go:154-182`). A slow peer check can therefore starve the
refresh, and 12 consecutive failures stop the session watcher.
*Fix:* move the writer onto `Env`, and give each phase its own budget.

## Documentation

**DOC-1 (Medium; executed; est9 reported it live).** Planning a crew needs the role and crew prompts:
handoff order, stop rules, and who reports to whom. They exist only as embedded files
(`internal/crew/builtin/**`, about 4,300 words). `crews` prints names, lead and members only
(`cmd/rex-amq-adapter/main.go:727-767`). `__internal` serves only `skill|watch|pane-title`
(`cmd/rex-amq-adapter/install.go:164-177`). Only `join` prints a briefing, and it relabels the pane.
The guide names the crews (`skill.md:223`) without their purpose, gives the 6-agent cap only under
"Summon on send" (`skill.md:291`), and has no recipe for a goal no built-in crew fits. In this run the
orchestrator spent five extra calls reading `internal/crew/builtin` before choosing roles. Agents
outside this repository cannot read those files at all.
*Fix:* add `crews show <crew|role>`, a one-line purpose per crew and role in `crews`, and a "Plan a
crew" section in the guide.

**DOC-2 (Medium, located).** `docs/design.md` has no section on the summon guards (request kinds,
cycle, depth 2, cap 6, grant/asked, token reservation). Section 9 (`:1239-1256`) omits
`internal/transcript` and `internal/trust`, and section 8 (`:1130-1161`) omits `title-state/`.
Section 4 runs 690 lines (`:151-837`) without subheadings. The guide omits the eight managed mail
verbs, `link`/`unlink` and `view`. README's command and state tables (`README.md:220-230,274-287`)
omit the same commands and several state dirs (`extensions/rex-amq-adapter/*`, `views/`, `summon/`,
`health/`, `turn-error/`).

## Testing

**TEST-1 (Medium, traced).** `.github/workflows/release.yml:15-29` installs only Go on
`ubuntu-latest`, and says so. `internal/e2e/harness_test.go:92-98`,
`internal/app/lifecycle_test.go:18-22` and `internal/amq/lifecycle_test.go:18-22` skip without Rex or
amq. At least 117 of 271 `Test*` functions in `internal/app` go through `realAmqEnv`. A green release
run therefore proves nothing about real delivery, wakers or summon guards. The CLI's flag dispatch is
barely tested (`cmd/rex-amq-adapter` 20.2%), which is how SEC-2 and A11Y-2 landed with passing gates.
*Fix:* a macOS integration job with pinned Rex and amq that fails when a required test skips, unit
tests for CLI flag dispatch, or the `amq` seam (MAINT-2).

Coverage (package-local, `go test -short -cover`) is 72.4% overall. By package: app 82.0%, core
83.6%, install 87.7%, screen 89.9%, trust 82.2%, amq 54.2%, store 51.9%, rex 17.9%, cmd 20.2%,
fsutil 0.0%. Low numbers for `cmd`, `rex` and `fsutil` partly reflect code exercised only through
subprocess-built binaries or as dependencies.

Strengths: table-driven core and screen tests, including CJK and emoji widths. Lifecycle tests run on
real amq wakers with generation-safe retirement. Summon tests cover cycles, depth, cap, pending
reservations and unknown ancestry. The e2e harness uses private Rex servers, private state and real amq.

## Automation

**AUTO-1 (Medium, located).** The only workflow triggers on `push.tags: ["v*"]`
(`.github/workflows/release.yml:6-8`) and runs vet and test, without race or a vulnerability scan.
The maintainer tests by hand, tags, then bumps Homebrew by hand (`README.md:304-308`).
*Fix:* a read-only push/PR workflow with build, vet, `-short`, `-race` and govulncheck.

**AUTO-5 (Medium per raiser, traced, unverified by a second auditor).** `who` calls
`SuperviseSession`, then `ensureWaker`, then `prepareInjector` (`cmd/rex-amq-adapter/main.go:398`,
`internal/app/lifecycle.go:45-62`). That rewrites `<state>/bin/inject` to exec whatever binary is
running. Running a scratch build's `who`, which users treat as read-only, against the default state
therefore repoints every live agent's doorbell at a temporary binary. The maintainer hit exactly this
on 2026-11-06, and this audit's briefs had to warn every agent about it.
*Fix:* pin the installed binary at `setup`; refuse to repoint `bin/inject` to a temp or unpinned
binary unless asked.

**AUTO-2 (Low, reproduced).** `who --json` exits 2. `crews --json` exits 0 with the text table,
because `runCrews` takes its first argument as a directory (`cmd/rex-amq-adapter/main.go:728-731`).
`--version` is an unknown command although `version` works (`main.go:154-156`).
*Fix:* `who --json` and `crews --json` snapshots, and reject unknown flags.

**AUTO-3 (Low, located).** README requires amq features (`README.md:46-47`) and a state wipe after
upgrades (`:66-67`). Nothing probes the amq version, the state carries no version stamp, and `status`
has no amq or Rex row. After `brew upgrade`, a mismatch fails inside hooks whose stderr goes to
`logs/hooks.log`, and agents simply stop hearing mail.

**AUTO-4 (Low, reproduced in this run).** The guide says `--timeout` changes `await`'s wait, but
`await <block> --timeout 60` exits 2. It needs `60s`.

## Accessibility

Scope: the CLI and terminal surface (pane labels, doorbell text typed into prompts, help and error
text, machine-readable output).

**A11Y-1 (Low after two downgrades, executed).** `internal/screen/promptbox.go:210-215` accepts a
titled Claude border only when it starts with ten `─`. Anything else becomes `deferred box_not_found`
(`internal/core/inject.go:131-132`), and the regression test uses 60 leading dashes
(`internal/screen/screen_test.go:132-148`). A classifier probe shows a narrow, title-first border
classified `unknown`. agy-01 added that widths of 40 to 59 columns also fall outside the watchdog's
tiny-pane recovery (`internal/core/watchdog.go:19-20,59-61`). codex-01 confirmed both. It kept Low
because no one has verified that current Claude Code renders that shape, the watchdog reports the
miss (`internal/app/watchdog.go:184-186`), and prompt hooks still deliver. Any fix must keep the
deliberate rejection of a similar transcript line (`screen_test.go:151-153`).

**A11Y-2 (Low, reproduced).** `title --help` stores `--help` as the task and renames the pane
(`cmd/rex-amq-adapter/main.go:362-369`). `crews --help` prints the table, and `join/events/view
--help` exit 2. Managed `send` without `--body` reads stdin before handling `--help`, so on a TTY it
waits forever (`internal/app/managed.go:121-131`, traced).
*Fix:* handle `-h/--help` before resolving the caller or writing state, exit 0 for help, and refuse an
implicit stdin read from a TTY.

**A11Y-3 (Low, executed).** A follower tab mirrors its single agent's pane label and freezes it once
peers join (`docs/design.md:305,314-316`; `internal/core/tab.go:72-91`). This audit's four-agent tab
was therefore named `orchestrator(claude-03) | crew audit goal`, and `who`'s TAB column read as if
every peer were that agent's pane. A disposable Rex probe showed that Rex never derives a window
label from a block rename or an OSC title, so the adapter set it. This is by design, but misleading.

Strength: task truncation handles double-width CJK and emoji. Per-rune counting of joined emoji is a
documented limitation (`internal/core/identity.go:201-204`).

## Gate results

codex-02 ran each gate once on the unchanged candidate, each with a fresh `REX_AMQ_ADAPTER_HOME`
(go1.27.4 darwin/arm64, amq 0.141.0, rex 0.0.1793981048-g01d6c4).

| Gate | Command | Result | Seconds |
|---|---|---|---:|
| build | `go build ./...` | pass | 0.7 |
| vet | `go vet ./...` | pass | 0.1 |
| short tests | `go test -short ./...` | pass | 176 |
| race | `go test -short -race ./...` | pass | 185 |
| coverage | `go test -short -coverprofile=... ./...` | pass, 72.4% | 178 |
| e2e | `go test -json ./internal/e2e/...` | pass, 20 top-level tests, 0 skipped | 12 |
| vulnerabilities | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | no vulnerabilities found | 11 |

Not run: the `REX_AMQ_LIVE=1` client tests, which create a tab on the user's live Rex server, and any
live narrow-pane Claude rendering check.

## Verification

In round 1, each auditor covered its own dimensions; agy-01 covered all nine, blind to the others.
In round 2, every High or Critical finding, plus the Mediums that touch trust, the gateway and summon,
went to an agent of another model family. That agent re-derived each finding from `file:line` and
re-ran or wrote probes in isolated state.

| Finding (this report) | Raised by | Verified by | Verdict | Evidence |
|---|---|---|---|---|
| ARCH-3 | agy-01 (Gemini), High | codex-02 (Codex) | CONFIRMED High; both mail paths refuse a valid directive | `verify2-codex-02.md` |
| ARCH-3 | claude-04 (Claude), independently | - | failing test on f86e7a1 | claude-04 report, `fix/agent-guidance` |
| SEC-2 | agy-01 (Gemini), Critical | codex-01 (Codex) | DOWNGRADE to Medium; send/reply executed, read disclosure not reproduced | `verify2-codex-01.md` |
| SEC-3 | agy-01 (Gemini), High | codex-01 (Codex) | DOWNGRADE to Medium; narrowed to argv `--` | `verify2-codex-01.md` |
| ARCH-1 (agy's additions) | agy-01 (Gemini), High | codex-02 (Codex) | DOWNGRADE to Medium, merged into ARCH-1 | `verify2-codex-02.md` |
| A11Y-1 (agy's 40-59 col argument) | agy-01 (Gemini), High | codex-01 (Codex) | DOWNGRADE to Low | `verify2-codex-01.md` |
| SEC-1 | codex-01 (Codex), Medium | claude-04 (Claude) | CONFIRMED Medium; repro re-run | `verify-claude-04.md` |
| SCAL-1 | codex-01 (Codex), Medium | claude-04 (Claude) | DOWNGRADE to Low | `verify-claude-04.md` |
| ARCH-1 | claude-04 (Claude), Medium | codex-01 (Codex) | CONFIRMED Medium; executed in isolated amq | `verify-codex-01.md` |
| ARCH-2 | claude-04 (Claude), Medium | codex-01 (Codex) | CONFIRMED Medium, narrowed to token ordering | `verify-codex-01.md` |
| A11Y-1 | codex-02 (Codex), Medium | codex-01 (Codex, same family) | DOWNGRADE to Low | `verify-codex-01.md` |

Every round-1 High or Critical was verified: one was confirmed and four were downgraded. Overlap
between agy-01's blind pass and the other three auditors also served as evidence, since it
independently re-found ARCH-1, A11Y-1, A11Y-2/AUTO-2, AUTO-1/TEST-1, PERF-1, MAINT-1 and DOC-2.

Not verified by a second agent: AUTO-1, TEST-1, AUTO-5, DOC-1, MAINT-1, DOC-2, SCAL-2, PERF-2,
MAINT-2, MAINT-3, SEC-4, PERF-1, A11Y-2, AUTO-2, AUTO-3, A11Y-3, AUTO-4. Each rests on its raiser's
located, traced or executed evidence.

## Out of scope

These were found while running the audit, outside this repository:

- The goal-contract skill (`make-goal`) names its scopes `push+PR | merge` after mechanisms rather
  than the authority they grant (propose vs. land). Its SCOPE section does not mention that the
  resident one-way-door rules still hold a push that would publish something sensitive (as here: the
  repository is public and this report lists unfixed findings), so the drafting agent invented a
  `local` scope instead. The hold itself works through the existing rules. See `crew-friction.md` F2.
