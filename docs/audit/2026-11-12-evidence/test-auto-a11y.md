# Testing, Automation, Accessibility audit

Candidate: `main@f86e7a195b3442c381434220f737b8d0561e61f5`. Tracked working-tree diff SHA-256: `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`. `.goal/` is pre-existing orchestrator-owned state. No repository source or tests changed. Required gates and environment are recorded in `/tmp/rex-audit-20261112/gates.md`; probe sources and raw output are under `/tmp/rex-audit-20261112/probe-codex-02/`.

## A11Y1 Narrow titled Claude input boxes are classified as missing

- Severity: Medium
- Dimension: Accessibility
- Evidence level: executed
- Evidence: `internal/screen/promptbox.go:210-215` accepts a titled border only if it starts with ten `─`; `ruledBox` at `:392-394` requires that border immediately above `❯`. `internal/core/inject.go:131-132` turns any non-empty/non-typed result into `deferred box_not_found`. The existing titled-box regression (`internal/screen/screen_test.go:132-148`) constructs **60** leading dashes and therefore misses title-dominated narrow borders. Run `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go run -mod=mod .` in the scratch `screen-probe/` module. `/tmp/rex-audit-20261112/probe-codex-02/screen-results.log` shows the same empty prompt/bottom border classified `empty` with ten leading dashes, but `unknown` with a title-first top edge or a single leading dash; a real draft in the narrow variant is also `unknown`.
- Impact: Idle Claude peers whose long session title occupies the upper edge of a split pane cannot receive a doorbell through this classifier; the diagnostic says the input box is absent even though the captured layout contains it. This audit exercised the exact classifier with screen text, not a live current Claude harness; current-version rendering incidence remains unverified. It fails closed, so this is lost wake delivery, not typing over a draft.
- Fix sketch: Extend Claude box recognition to a title-bearing top row using the paired prompt and bottom border as constraints, preserving rejection of transcript text and drafts. Add narrow title-first, short-rule, CJK/emoji title, empty and drafted captures to the regression table.
- Blast radius: Claude VT input-box recognition, doorbell readiness/confirmation, and watchdog diagnostics; no CLI or state-format change.

## AUTO1 Pull requests and ordinary pushes have no automated verification gate

- Severity: Medium
- Dimension: Automation
- Evidence level: located
- Evidence: `rg --files --hidden .github` finds only `.github/workflows/release.yml`; its trigger at `:6-8` is `push.tags: ["v*"]`. It runs `go vet ./...` and `go test ./...` at `:23-29`, but no race scan, formatting/lint gate, or vulnerability scan. README.md:304-308 instructs the maintainer to run tests, publish a tag, then bump Homebrew by hand.
- Impact: A normal branch push or PR can merge with compilation, race, or regression failures undetected by repository automation. The first automated feedback is attached to a release tag, after the intended release revision has already been selected.
- Fix sketch: Add a read-only push/PR workflow for build, vet, short tests and race tests; keep publishing permissions and release tagging in the release job. Link/check the verification job from the release path. Treat Homebrew bump automation as a separate optional improvement, not a correctness prerequisite.
- Blast radius: GitHub workflow triggers and build/release gates; no runtime API/state change.

## TEST1 Release CI silently skips the real delivery and waker suites

- Severity: Medium
- Dimension: Testing
- Evidence level: traced
- Evidence: `.github/workflows/release.yml:15-29` uses `ubuntu-latest` and installs Go only; its own comment explicitly expects Rex/amq-dependent tests to skip. `internal/e2e/harness_test.go:92-98` calls `t.Skip` when either binary is absent. `internal/app/lifecycle_test.go:18-22` also skips every caller of `realAmqEnv` without amq, which includes delivery lifecycle, summon guard and title tests. `internal/amq/lifecycle_test.go:18-22` skips the real amq contract test. Thus a green `go test ./...` in that workflow does not require any real message delivery/waker lifecycle execution.
- Impact: Release verification cannot detect an incompatible Rex CLI/amq release, failed real wake startup, or broken delivery even though the repository has suitable integration tests. This is a workflow coverage gap; it does not claim the locally executed tests failed.
- Fix sketch: Provision a macOS integration runner with pinned supported Rex/amq versions, run the suite alone, and fail the integration job if required tests are skipped. A preflight binary/version check should distinguish an unavailable environment from a passing integration gate.
- Blast radius: CI dependency provisioning, supported external tool versions and release verification; no production state-format change.

## A11Y2 Subcommand help can change a task instead of explaining the command

- Severity: Low
- Dimension: Accessibility
- Evidence level: reproduced
- Evidence: `cmd/rex-amq-adapter/main.go:362-369` passes all title arguments to `SetTask` without parsing help. `:727-735` treats the first crews argument as a directory. Run `/tmp/rex-audit-20261112/probe-codex-02/probe-cli.py` (exact candidate binary, isolated adapter state, fake Rex). In `cli-results.json`, `title --help` exits 0, prints `qa(codex-qa-probe) | --help` and changes the stored task from `audit candidate` to `--help`; fake-rex.log records the rename invocation. `crews --help` exits 0 and prints the crew table instead of usage. `join/events/view --help` print help but exit 2.
- Impact: A person or agent probing a familiar `--help` surface unintentionally overwrites its display task, or receives a listing for an unintended directory instead of usage. The label mutation is easy to undo, but undermines safe command discovery.
- Fix sketch: Handle `-h`/`--help` before resolving the caller or mutating state, return exit 0 for help, and validate unexpected flags consistently. Preserve tasks beginning with a dash through an explicit `--` delimiter.
- Blast radius: CLI help/exit-code semantics for title, crews and flag-based subcommands; no on-disk schema change.

## AUTO2 Discovery offers no structured snapshot and silently accepts an unsupported JSON flag

- Severity: Low
- Dimension: Automation
- Evidence level: reproduced
- Evidence: `cmd/rex-amq-adapter/main.go:377-381` defines only `who --all`; `:727-735` treats `--json` as crews' directory. `cli-results.json` records `who --json` -> exit 2, `flag provided but not defined: -json`; `crews --json` -> exit 0 with the ordinary text table. `events --json` is a different streaming interface, while await and installation already emit JSON.
- Impact: Agent/tooling clients must parse aligned human tables or manage a streaming events process merely to obtain a discovery snapshot. A client probing crews' JSON support can mistake success for machine-readable support, and project-local crews are looked up relative to a directory literally named `--json`.
- Fix sketch: Add explicit `--json` snapshots for who/crews and reject unknown flags. Document their schemas and scope; retain the text default for interactive use.
- Blast radius: Additive discovery CLI flags/output schemas; flag rejection changes invalid-input behavior.

## Validation and coverage assessment

All seven required gates passed on the unchanged candidate; no gate failure or e2e retry occurred. The private e2e run produced 20 top-level / 24 total test pass events, zero skips and no cached result. `govulncheck` exited 0 with `No vulnerabilities found.` See `gates.md` for exact commands, exit codes, durations, package coverage and the probe result matrix.

Total statement coverage is 72.4%; app 82.0%, core 83.6%, install 87.7%, screen 89.9%, amq 54.2%, store 51.9%, Rex CLI client 17.9% and command package 20.2%. The command suite records 0% for `runTitle` and `runCrews`, consistent with the reproduced help holes; however other 0% values (for example `runManaged`) can arise because its tests build a subprocess binary without `-cover`. Do not use those numbers alone to infer missing behavior tests. `SetTask` is 77.8%, `Summon` 90.3% and `CanExchange` 100%; their remaining percentages do not prove a specific defect. Lock contention is already tested by lifecycle/notification and concurrent-drain tests; fsutil has no dedicated package tests, so its package-local 0% does not mean unexercised locks.

The e2e collector retained VT captures, sessions/blocks JSON and hook/doorbell `.got` files for 20 private servers (21 VT snapshots, only one showing the scripted-agent box; the rest are startup/shell state, plus 12 received-doorbell files) under `probe-codex-02/e2e-surface/`. `e2e-summary.json` lists the actual received doorbells, which name `$TMPDIR/rae-adapter-64277`, proving these scripted terminals used the candidate-built adapter, not the installed one. The source SHA and tracked diff hash still match the initial identity.

## Strengths

- Core policy and terminal classification have dedicated table-driven tests, including CJK and fixed-width emoji title boundaries.
- Lifecycle tests use real amq mailbox/waker processes and check generation-safe retirement; summon tests cover cycles, depth, caps, pending reservations and unknown ancestry.
- The e2e harness builds the candidate binary and starts private Rex servers, adapter state and real amq roots rather than using the machine's live agents.
- Installation tests cover idempotency, append-only changes, foreign content preservation, dry runs, symlinked settings, upgrade and removal.

## Not covered

- No source, test, hook, installation, user state, or live pane changes were authorized or made. No failure was fixed.
- No real-model harness/UI matrix, screen reader, color-vision user study or current Claude narrow-pane rendering experiment was performed. Narrow-box evidence is an executed classifier probe.
- Per-rune counting of joined emoji is explicitly documented in `internal/core/identity.go:201-204`; it is a known limitation, not raised here as a newly discovered defect.
- Homebrew publication was inspected from workflow/docs only; no tag, release, formula bump or remote mutation was performed.
- Coverage is package-local; dependency calls and child binaries are not included in their caller's coverage counters. Therefore a 0% lock/helper number is not evidence that no integration test exercised it.
