# Round 2 independent verification

Target remains `main@f86e7a195b3442c381434220f737b8d0561e61f5`; `git diff --exit-code` passed after the probes. Only the requested ARCH-1, ARCH-2, and A11Y1 were re-derived. `agy-all.md` was not read.

The independent reproducer is `/tmp/rex-audit-20261112/probe-codex-01/round2.go`, importing the candidate's real Go packages. Run from its directory:

```sh
eval "$(/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter env)" && REX_AMQ_ADAPTER_HOME=/tmp/rex-audit-20261112/probe-codex-01/state go run -mod=mod . round2
```

AMQ operations name new scratch roots explicitly. The drain and token probes use the installed AMQ binary for isolated mailbox setup/read checks; the token probe uses Pi's in-process adoption path, so no waker is spawned. Rex calls use an in-memory `app.Terminal`. No live pane, installed hook, real trust setting, repository source/test, or existing adapter state was changed. No full test/vet suite was run.

## ARCH-1 — CONFIRMED (Medium)

**Reason:** The default instructions select bare AMQ, which can consume forbidden mail arriving after a prior arrival-gate pass; the managed drain independently rechecks admission and does not expose that same mail.

**Independent source evidence:** `internal/core/announce.go:103` constructs the hook's bare `amq drain` instruction; `internal/core/announce.go:117` teaches bare `amq send`; `internal/core/inject.go:189` puts bare drain into the doorbell. `internal/app/adopt.go:476` exports only `AM_ROOT` and `AM_ME`, with no alias, executable wrapper, or PATH rewrite. In contrast, `internal/app/managed.go:194` locks consumption, `:199` runs admission, and `:207` consumes only admitted IDs. `internal/app/gateway.go:89` fails closed on bad headers and `:100` quarantines denied messages. The narrator policy denies ordinary member mail at `internal/core/gateway.go:146`.

**Independent execution:** `drainProbe` creates one member and one narrator in a scratch session/root. A managed empty drain completes a prior gate pass. Then the member sends ordinary, non-system mail, and raw AMQ drain reads it. A second message with the same sender/recipient/policy is instead sent through the managed drain: it is quarantined, no body is returned, and count is zero. Output:

```text
DRAIN: policy=narrator_takes_no_member_mail raw_count=1 raw_body_exposed=true managed_count=0 quarantined=1
```

This establishes the protocol/instruction mismatch and the drain-time protection difference. It does not claim an OS sandbox, nor that all arrival hooks are absent. The report's provenance concern is also source-backed: `internal/app/gateway.go:48` falls back to the sender's current endpoint when no adapter provenance exists, while `:171` builds send-time provenance for managed sends. The raw fallback is explicitly documented, so retain the finding around the inconsistent prescribed path, not as an undisclosed general ability to forge UNIX-user mail.

**Fix direction:** Teach/use the adapter's managed mail commands consistently where gateway guarantees are promised. Keep direct AMQ as an explicitly weaker administrative path if it remains supported.

## ARCH-2 — CONFIRMED (Medium), with scope correction

**Reason:** An actual failed record write after `TakeSummon` loses both the reservation and lineage; a successful retry adopts the same launched session as depth 0. This is sufficient for the Medium finding even without treating raw Rex as a security sandbox.

**Independent source evidence:** `internal/store/store.go:855` removes the reservation as part of `TakeSummon`; `internal/app/adopt.go:206` calls it and copies lineage at `:209`; the first authoritative record is written later at `:220`. A fresh record has zero parent/depth unless restored from an existing record (`:200`) or that token. `internal/app/summon.go:203` treats an empty parent as the end of a complete chain; `internal/app/summon.go:239` passes the recorded depth into the guards.

**Independent execution:** `tokenProbe(false)` verifies normal adoption preserves `Parent="claude-01", Depth=2`. `tokenProbe(true)` injects a deterministic filesystem failure after initial observation but before the first record write: in a new scratch state directory, a regular file temporarily occupies the `records` directory path. `Adopt` then fails writing the record, but the token has already disappeared and pending count is zero. After removing only that scratch blocker, the same session/token retries successfully with empty parent/depth 0. Output:

```text
TOKEN: failed_PutRecord=false parent="claude-01" depth=2
TOKEN: failed_PutRecord=true token_survives=false pending=0 retry_parent="" retry_depth=0
```

The injected filesystem failure is a deterministic stand-in for a failing `PutRecord` or process death in that transfer window. It proves recovery semantics, not the incidence of write failures on the live machine.

**Launch-route qualification:** `tab` really does call `NewWindow` directly at `cmd/rex-amq-adapter/main.go:567`, without `GuardSummon` or a summon token. A tokenless fresh adoption defaults to parent empty/depth 0. However `core/summon.go:14` defines the cap as when a **summon** refuses, and the installed guide explicitly authorizes raw `rex split`/`run` recipes. Neither raw Rex nor `tab` was a sandboxed launch boundary. Thus the broad claim that every manually launched agent must be bounded is not independently established; report this part as a command-policy/guide gap if a universal launch limit is intended. Do not claim the adapter can prevent a same-user process using raw Rex. No new live `tab`/split launch was needed or performed.

**Fix direction:** Retain the token until the record containing its lineage is successfully committed; remove it afterwards while holding the same lifecycle lock. On retries, preserve already-committed lineage. Specify separately whether agent-originated `tab` must share summon admission; changing that policy is a CLI behavior decision, not necessary to fix the proven token-loss bug.

## A11Y1 — DOWNGRADE to Low

**Reason:** The classifier false negative for a synthetic title border with fewer than ten leading dashes is reproducible, but actual current-Claude rendering of that shape is unverified; the existing negative test intentionally rejects a similar short-rule/transcript shape.

**Independent source evidence:** `internal/screen/promptbox.go:215` requires ten leading `─` for a titled border; `:392` requires such a border immediately above the prompt. `internal/core/inject.go:131` defers an unknown box with `box_not_found`. The existing positive titled-border test uses 60 leading dashes (`internal/screen/screen_test.go:133`), while `:151`–`:153` explicitly reject `── note` above a prompt and lower rule.

**Independent execution:** `narrowProbe` holds the prompt and 40-dash lower edge constant, varies only the top edge's leading-rule length, and checks both classification and the real write-readiness policy. Output:

```text
NARROW: leading_rule=10 draft=false box=empty write_allowed=true reason=
NARROW: leading_rule=10 draft=true box=typed write_allowed=false reason=draft_in_box
NARROW: leading_rule=9 draft=false box=unknown write_allowed=false reason=box_not_found
NARROW: leading_rule=9 draft=true box=unknown write_allowed=false reason=box_not_found
NARROW: leading_rule=1 draft=false box=unknown write_allowed=false reason=box_not_found
NARROW: leading_rule=1 draft=true box=unknown write_allowed=false reason=box_not_found
NARROW: transcript-like short-rule control remains unknown
```

This proves the recognition boundary and its fail-closed outcome. It does not establish how frequently, or whether, the current Claude renderer produces the synthetic short-prefix title border. Without that evidence, the Medium impact statement about real split-pane wake loss is conditional. The false negative also does not imply draft corruption: every unrecognized variant refused a write.

**Fix direction:** Obtain an authentic narrow titled Claude VT capture, then improve recognition against that fixture while preserving the existing transcript negative control. Merely lowering the prefix threshold would weaken a tested safety condition. No live harness capture was authorized or attempted in this round.
