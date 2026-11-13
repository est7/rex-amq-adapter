# Findings: Architecture, Maintainability, Documentation (claude-04)

Target: /Users/est9/EstProjects/rex-amq-adapter @ f86e7a195b3442c381434220f737b8d0561e61f5.
Installed binary `/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter version` = v0.12.0 = HEAD
(0 commits after the tag). `__internal skill` output is byte-identical to internal/guide/skill.md.
Probes: /tmp/rex-audit-20261112/probe-claude-04/ (funclen = go/ast function-length probe; crews = project-crew probe).

Several structural items are already in docs/backlog.md (U1 harness facts, U2 no amq seam,
U4 decisions inside app, U5 duplication). They are cited by U-ID below and not re-filed as new
findings. Every finding here is either new or adds evidence the backlog lacks.

## ARCH-1 The mail path agents are told to use skips the gateway's managed commands
- Severity: Medium
- Dimension: Architecture
- Evidence level: traced (plus executed: `env` output and `type amq`)
- Evidence:
  - design.md:1178-1193 names the gateway-checked path: the managed `send`/`reply` record send-time
    provenance, and the managed `drain` "reads by exact id only the mail its own gate admitted". It also
    says "Mail sent with `amq` directly has no provenance and is judged by its sender's record now".
  - Implementation: managed.go:20-61 (`ManagedMail`) and managed.go:181-212 (drain = `LockConsume`, then
    `admit`, then consume only `kept` ids). managed.go:214-216 (`afterAdmit`) is the test seam for exactly
    that race. Size: managed.go 465 lines + gateway.go 393 lines.
  - Every instruction surface tells agents to use bare `amq` instead:
    - doorbell text: core/inject.go:189 (`amq drain --include-body`);
    - hook notices: core/announce.go:103 and core/announce.go:121;
    - summon resume bootstrap: app/summon.go:278;
    - started-agent brief: app/summon.go:325 (`amq send` / `amq reply`);
    - guide: skill.md:82 and skill.md:123;
    - 6 role prompts, including narrator.md:4 (`amq drain --include-body`), the gateway's main protected
      party.
  - `grep -rn 'rex-amq-adapter \(send\|drain\|reply\|read\)' internal/crew/builtin docs README.md internal/guide` returns nothing.
  - Executed: `rex-amq-adapter env` prints only `export AM_ROOT=… AM_ME='claude-04'`, and after the eval
    `type amq` gives `/opt/homebrew/bin/amq`. Nothing shims bare `amq`.
- Impact: on the instructed path, two of the gateway's own guarantees do not hold.
  - (a) Drain: a bare `amq drain` consumes mail that arrived after the last arrival-gate pass (gate passes
    run at the injector and hooks, design.md:1179-1181). For a narrator or personal agent this includes
    mail the gate would quarantine. This is the race the managed drain exists to close.
  - (b) Send: sends carry no provenance, so a sender that changes session before arrival is judged by its
    new record and the generation fence can misjudge.
  - Arrival gating still runs, so this is a weakened guarantee, not an open door. design.md:1195 itself
    calls the gateway "a policy on the supported paths". The supported path is simply not the one
    anybody is taught.
- Fix sketch: pick one path and make every surface agree.
  - Recommended: change the four notice strings (core/inject.go:189, core/announce.go:103 and :121,
    app/summon.go:278/325), skill.md, and the role prompts to `rex-amq-adapter drain --include-body` /
    `rex-amq-adapter send|reply`.
  - Otherwise: if bare `amq` is intended, delete the managed verbs and state in design.md that the
    drain-time gate is not offered.
- Blast radius: doorbell and notice text (the model reads it, and core tests asserting the exact strings
  change); guide; role prompts. The CLI surface stays the same.

## ARCH-2 Summon guards are checked by one command, not enforced at launch or adoption
- Severity: Medium
- Dimension: Architecture
- Evidence level: traced
- Evidence:
  - `core.GuardSummon` has exactly one call site, app/summon.go:239 (inside `reserve`). Found by
    `grep -rn 'SummonCap\|GuardSummon('`.
  - `tab` launches with no guard. It calls `rex.Client.NewWindow` directly from cmd
    (cmd/rex-amq-adapter/main.go:525-579, client built at :566).
  - The guide's primary launch recipes use raw `rex split` / `rex run` / `rex new` (skill.md:215-221).
  - Adoption assigns lineage only from an existing record or a summon token (app/adopt.go:200-210). An
    agent started any other way is recorded `Parent=""`, `Depth=0`. `chain()` then treats it as complete
    (app/summon.go:202-204), so it passes the depth and cycle guards as if the user had started it.
  - Second path to the same reset: `TakeSummon` deletes the token file (store/store.go:855) at
    adopt.go:206, before the record is committed at adopt.go:220. If `PutRecord` fails, the retrying hook
    finds no token and the summoned agent is re-adopted at depth 0. Its cap reservation is also released
    early.
- Impact: suppose a depth-2 summoned agent follows its own guide's split recipe. It creates a depth-0 agent
  that may summon again. Through `tab`, `rex split`, or `rex new` (a fresh session gets a fresh cap of 6),
  the 6-agent cap and the grant/asked requirement are skipped. core/summon.go:9-11 states the intent as
  "trust the main agent; cap the count, refuse cycles, limit depth". These paths extend that trust to
  summoned agents too.
- Fix sketch:
  - (1) Route `tab` through the same `reserve` and token as `summon`, by moving runTab's launch into app
    (this also folds backlog U5's duplicated trust derivation).
  - (2) In the identity notice and skill.md, tell an agent with `Depth >= 1` to start peers only through
    `summon`/`tab`, never raw `rex split`.
  - (3) In Adopt, read the token, commit the record, then delete the token.
- Blast radius: `tab` may start refusing (exit code and stderr change); guide text; record lineage
  semantics. No on-disk format change.

## DOC-1 The agent guide cannot plan a crew for a goal without reading the source (evidence for est9's report)
- Severity: Medium
- Dimension: Documentation
- Evidence level: executed (the CLI probes) and located
- Evidence:
  - No read-only way to read a role's or crew's prompt.
    - `crews` prints only names, lead and members (cmd/rex-amq-adapter/main.go:727-767).
    - `__internal` serves only `skill|watch|pane-title` (cmd/rex-amq-adapter/install.go:164-177).
    - Only `join` prints a briefing, and it renames the tab and relabels the pane (`--help` text).
    - The behaviour a planner needs (handoff order, stop rules, who reports to whom) is in
      internal/crew/builtin/**/*.md: 4,294 words, readable only from source.
  - The guide names the crews but never says what each does. skill.md:223 lists ``pair`, `ship`, `dev`,
    `docs`, `design`` only, and the one-line purpose table exists only in README.md.
  - `crews` semantics are unexplained.
    - `design` shows ROLES `architect, architect` (a count?).
    - `docs` shows LEAD `architect`, but docs/team-conventions.md body says "With an architect in the tab,
      the architect leads … Without one, the writer the human briefed leads". The header's lead is static.
      Nothing marks a member as optional.
  - Capacity rules are scattered.
    - The 6-agent cap appears only under "Summon on send" (skill.md:291).
    - Stealth roles are excluded from it (app/summon.go:232), which the guide does not say.
    - It does not apply to `rex split` launches at all (ARCH-2).
    - The default harness per role is likewise only under summon (skill.md:277-279).
  - Nothing in skill.md maps a goal's shape to a crew, or says how to run several Slices (one tab per
    crew? `summon --cwd <worktree>` per Slice?). That guidance exists only in roles/orchestrator.md, which
    is unreachable read-only (see the first bullet).
  - No machine-readable discovery (executed):
    - `rex-amq-adapter who --json` gives `flag provided but not defined: -json`.
    - `rex-amq-adapter crews --json` exits 0 and prints the table. runCrews takes any first argument,
      flags included, as the directory (main.go:728-731).
- Impact: an orchestrator planning a crew must read the repository's source, or launch with `join` and
  learn the conventions after the side effect. It cannot check crew size against the cap before starting
  agents.
- Fix sketch:
  - Add `crews show <crew|role> [dir]`, read-only, printing the resolved prompt and its layer.
  - Add `crews --json` and `who --json` (members, lead, optional flag, default harness per member).
  - Make runCrews reject unknown flags.
  - Add a "Plan a crew" section to skill.md: a crew table (purpose, members, lead, default harness,
    agent count against the cap of 6, stealth exclusion) and a goal-to-crew recipe (one crew per tab,
    `summon --cwd` per Slice worktree, the orchestrator outside the crew).
- Blast radius: new subcommand and flags (additive); guide text; `crews` stops accepting a flag-shaped
  directory.

## DOC-2 design.md (the design source) has drifted from the code in four places
- Severity: Medium
- Dimension: Documentation
- Evidence level: located (each a grep that found nothing, or a path comparison)
- Evidence:
  - The summon guard model is absent.
    - `grep -n -w -i 'depth\|grant\|asked\|cycle\|GuardSummon\|SummonCap\|REX_AMQ_SUMMON' docs/design.md`
      has no hit about summon (the hits are "asked for a task" and "lifecycle").
    - Summon appears only in passing (design.md:307, 310, 957, 1148, 1153, 1178, 1204).
    - The guards (request kinds, cycle, depth 2, cap 6, grant/asked, untraceable-chain refusal, token
      reservation) are specified only in README.md, skill.md:283-296, and core/summon.go.
    - The feature landed in 7b303f8 (2026-11-09).
  - §9 Module layout (design.md:1239-1256) omits the `internal/transcript` and `internal/trust` packages,
    which both exist with their own package docs.
  - §8 Paths (design.md:1130-1161) omits `title-state/` (store/store.go:48), which the live state dir
    contains. All 14 other `filepath.Join(p.Home, …)` dirs are listed.
  - §4 "Identity" spans design.md:151-837 (about 7,700 words) with no `###` subheadings. Inside it sit
    crews (:565), orchestrators (:625), the watchdog (:690), and the events stream (:799), so none of them
    has an anchor or a TOC entry.
- Impact: a maintainer changing the cap, depth or grant rules has no design statement to check against.
  The module map misleads anyone placing new code; the paths list misses one dir a state wipe must cover.
- Fix sketch:
  - Add a §4.x "Summon" section: the guards, their order, the token lifecycle, and the gap in ARCH-2.
  - Add transcript and trust to §9, and title-state to §8.
  - Promote the bold lead-ins in §4 to `###` headings, or move crews/orchestrator/watchdog/events into
    their own top-level sections.
- Blast radius: docs only.

## MAINT-1 The add-an-agent-kind procedure misses the summon launch table, which falls back to Claude silently
- Severity: Medium
- Dimension: Maintainability
- Evidence level: traced
- Evidence:
  - docs/adding-an-agent-kind.md §4 (:150-200) lists identity.go, harness.go, transcript.go,
    app/title.go, promptbox.go, env.go, hook.go, caller.go, install.go, and skill.md. `grep -n
    'startCommand\|resumeCommand\|DefaultHarness\|TakesTitle\|summon' docs/adding-an-agent-kind.md` finds
    nothing.
  - `startCommand` ends in `return []string{"claude", brief}` for any unlisted kind
    (app/summon.go:400-410). `--harness` is validated only by `core.ParseKind` (app/summon.go:318-320),
    which is checklist step 1.
  - Other per-kind branches the checklist never names: `core.Kind.TakesTitle` (core/title.go:52),
    app/lease.go:110, app/inject.go:137, app/lineage.go:31, app/health.go:154-157,
    install/tasks.go and install/supervisor.go, cmd/rex-amq-adapter/main.go:217 and install.go:157.
  - Same regex at both commits: per-kind branch lines grew 66 → 74 and files 16 → 18 from 9a12346 to
    HEAD (`git grep -P 'core\.(Agy|Pi|Claude|Codex)\b|case (…)\b|== (…)\b'`).
- Impact: a maintainer who follows the procedure gets a kind that passes `ParseKind`. Then
  `summon <role> --harness <new>` starts Claude Code with the brief under the new role's label. The summon
  reports `start`, so nothing fails. A departed agent of the new kind gets a "cannot be resumed from
  outside" refusal (summon.go:279-284) that the docs never predict.
- Fix sketch:
  - Make startCommand/resumeCommand return an error for an unlisted kind.
  - Add a step "Launch: `startCommand`/`resumeCommand` in app/summon.go, `DefaultHarness`, `TakesTitle`"
    to the checklist.
  - Longer term, backlog U1's per-kind static-fact table with a test that iterates every `Kind` and fails
    on a zero field.
- Blast radius: internal only; summon's refusal text for an unknown kind.

## MAINT-2 The backlog's premise for deferring refactors is stale, while the debt keeps growing
- Severity: Medium
- Dimension: Maintainability
- Evidence level: executed (git measurements)
- Evidence:
  - Since 9a12346 (2026-11-10, the commit the backlog's line refs were taken at), `internal/app` non-test
    source grew from 5,621 to 7,795 lines (+39%), and `func (e Env)` methods from 134 to 175 (+31%).
    Measured with `git grep -h '^func (e Env)' <rev> -- 'internal/app/*.go'`.
  - The 35 commits after the backlog was written (18633c4) added the gateway, managed mail, view and
    narrator subsystems inside `app` (+2,327 lines, `git diff --stat 18633c4 HEAD -- internal/app`).
  - backlog.md:65-70 rejects splitting `Env` at "≈131 methods".
  - backlog.md:9-13 defers U1/U2/U4 until the lifespan decision (agent-post is to replace the adapter;
    Mem decision 2026-11-02: agent-post v1 ports crews, roles and await).
  - U1's ref `app/summon.go:362-391` is now :400-429.
  - U2, test seam: at least 117 of 271 `Test*` funcs in internal/app (26 of 36 files) call
    `realAmqEnv(t`, which skips without amq (app/lifecycle_test.go:17-22). release.yml installs no amq.
    This is a lower bound from direct calls only, consistent with the backlog's "about 75%" counted
    another way.
  - Largest functions (go/ast probe): `(Env).Adopt` 232 lines (app/adopt.go:39) carrying 6 interacting
    booleans (hasOwn/moved/again/unfinished/arrived/known); `inputBox` 130; `(Env).Summon` 110;
    `(Env).buildView` 110. Only 15 of 741 funcs exceed 60 lines.
- Impact: "wait for the lifespan decision" has become "keep adding features without seams". Each new
  subsystem raises U2/U4's cost and widens the untested surface that release CI skips. If agent-post
  ports crews, roles and await, the knowledge in those subsystems has no seam to lift it out by.
- Fix sketch: make the lifespan call explicit in backlog.md.
  - Either freeze the adapter to bug fixes and land new subsystems in agent-post,
  - or schedule U2 now: an `amq` interface beside `Terminal`, a pure call-site refactor that unblocks U4
    and lets CI run the 117+ tests.
  - Re-measure the backlog's numbers in place (Env count, U1 refs, U2 share).
- Blast radius: none for code; process and docs.

## AUTO-1 No check of the amq version or the state format, though README requires both by hand
- Severity: Low
- Dimension: Automation
- Evidence level: located; executed for `status`
- Evidence:
  - README.md:46-47 requires `amq send --labels`, `wake check --json-schema 2`, and
    `retire --if-generation`. The code just uses them (internal/amq/wake.go:195, :264), with no version or
    capability probe anywhere. `grep -rn 'json-schema\|if-generation\|--version'` shows only the call
    sites.
  - `rex-amq-adapter status` (executed) reports only harness items (skill/hook/supervisor/tasks/title):
    no amq row and no rex row.
  - README.md:66-67 says the adapter keeps no compatibility with earlier state and the user must wipe
    the state dir after an upgrade. But `store.Record` (store/store.go:68) and the state dir carry no
    version stamp (`grep DisallowUnknownFields|version` in store/app/install finds none).
  - Hooks run the stable opt path (README.md:58-59), so `brew upgrade` puts new code on old state at the
    next hook, and hook stderr goes only to logs/hooks.log.
- Impact: an amq that is too old, or a forgotten state wipe, fails inside hooks where nobody sees it.
  Agents simply stop hearing mail.
- Fix sketch:
  - `status` adds `amq` and `rex` rows (path, version, flag probe) and a `state` row.
  - `setup` writes `state-version`; a hook that sees a mismatch emits one in-band notice naming the wipe
    command instead of misreading.
- Blast radius: `status --json` gains items; a new file in the state dir.

## AUTO-2 CLI flag handling is inconsistent for agents that script it
- Severity: Low
- Dimension: Automation
- Evidence level: executed
- Evidence:
  - `who --json` gives `flag provided but not defined: -json` (exit 2).
  - `crews --json` and `crews --help` print the table, exit 0 (runCrews main.go:728-731 takes them as
    the directory).
  - `rex-amq-adapter --version` gives `unknown command "--version"` while `version` works
    (main.go:154-156).
  - Meanwhile `events`, `await`, `setup/status --json`, and `tab` emit JSON. An orchestrator wanting
    session membership must parse `who`'s table or start a stream.
- Impact: friction and silent misparse for agents. This audit's own orchestrator path used the table
  output.
- Fix sketch: `who --json` (the same rows as `events`' snapshot); reject unknown flags in `crews`; alias
  `--version`.
- Blast radius: additive flags; `crews` rejects flag-shaped args.

## Strengths
- `internal/core` is genuinely I/O-free: no `os`, `os/exec`, `net` or `time.Now()` in non-test files.
  The import graph is acyclic with core at the bottom (core → screen only; app → everything; cmd → app/core/crew/guide/install/rex/store/trust).
- The coupling to amq's private on-disk format is documented, bounded to read-only observers, and
  schema-checked: design.md:65-86, internal/amq/mailbox.go:31.
- Adopt is crash-safe by construction. It writes `Unfinished=Adopting` before starting the waker and
  clears it last (app/adopt.go:212-268), so a later hook finishes an interrupted adoption.
- Rex sits behind a narrow `Terminal` interface (app/env.go:22-44), and the rex client is a thin CLI
  wrapper with a stated rationale (internal/rex/rex.go package doc).
- docs/backlog.md records rejected designs with their reasons. That is rare and useful; this audit only
  updates its numbers.

## Not covered
- Read in detail: store.go (only Record and TakeSummon), hook.go (except kind branches), watchdog.go,
  health.go, install.go past the agent table and runInstall, and view/narrator correctness. State-model
  invariants beyond Adopt and summon were not traced.
- `go test` / `go vet` were not run (the QA peer owns the gates); the test counts are static grep and AST
  counts.
- Disclosure: I ran the installed `rex-amq-adapter status`, which is outside the brief's read-only list
  (help, crews, who, env). Afterwards I verified it read-only: cmd/rex-amq-adapter/install.go:58-59 calls
  `install.Status`, with no setup or remove path. I also ran `version` and `__internal skill`, both
  print-only.
- No dev-build probe was run, so REX_AMQ_ADAPTER_HOME isolation was not needed. The probes ran only the
  installed binary's read-only commands and a go/ast program outside the repo.
