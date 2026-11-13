# Round 2b: cross-family verification of agy-01's escalations

Candidate: `main@f86e7a195b3442c381434220f737b8d0561e61f5`. Scope: agy-01 SEC-1, SEC-2, A11Y-1 only. These IDs refer to `agy-all.md`, not similarly named findings in the other reports.

| Finding | Verdict | Independently established issue |
| --- | --- | --- |
| SEC-1 (Critical) | **DOWNGRADE to Medium** | Managed `send`/`reply` allow an actual caller/child parser differential and impersonated cross-Epic delivery; no new UNIX-user privilege boundary is crossed. The claimed inbound disclosure across every listed verb was not established. |
| SEC-2 (High) | **DOWNGRADE to Medium**, narrow scope | A dash-leading literal brief can become a harness option and prevent startup. Antigravity's skip flag and alternative `--asked` attestation are declared policy, not independently established privilege escalation. A directory accepted as a grant is a smaller validation defect. |
| A11Y-1 (High) | **DOWNGRADE to Low** | The synthetic short-border shape is unrecognized and widths at least 40 do not trigger resize, but current-Claude production rendering of that shape remains unverified. The watcher reports rather than silently abandoning it. |

All Go probes import the real candidate packages from `/tmp/rex-audit-20261112/probe-codex-01/round2b.go`; Rex launch operations use an in-memory `app.Terminal`. AMQ setup/sends/drains name fresh scratch roots. No live agent, waker, pane, trust store, or repository source/test was changed. Full test/vet runs remain outside this verifier's assignment.

Run the Go reproducer from its directory:

```sh
eval "$(/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter env)" && REX_AMQ_ADAPTER_HOME=/tmp/rex-audit-20261112/probe-codex-01/state go run -mod=mod . round2b
```

## SEC-1 — DOWNGRADE to Medium

**Confirmed mechanism (executed).** `internal/app/managed.go:445` treats every standalone `--` as an end-of-options marker while scanning for an arbitrary flag, without accounting for other flags' value arity. Native AMQ consumes `--` as the string value of `--subject`, then parses subsequent flags. `ManagedMail` checks routing before this at `:42`; caller-provided arguments are eventually appended after the adapter's own routing/context arguments at `internal/app/gateway.go:191`. The CLI passes its remaining arguments into `ManagedMail` at `cmd/rex-amq-adapter/main.go:826`.

The independent fixture has a real caller and local peer in Epic A and a different sender/recipient in Epic B. A normal managed send from A to B is refused. The following **argument array** first passes the gateway as a send to the local peer, then is interpreted differently by the child:

```text
--to codex-01 --body isolated-routing-override --subject --
--me claude-02 --to codex-02 --context {} --json=false
```

Here `--subject`'s value is the literal string `--`; these are separate argv entries, not a shell command. Overriding `--json=false` makes `amq.Post` fail to extract a sent ID, while the forged context has removed the pending token. No committed provenance is written (`internal/app/gateway.go:197`), and incoming `judge` falls back to the claimed sender's current endpoint (`:47`–`:51`). The receiver's real managed drain then admits the message as its own Epic's sender.

The same mechanism on managed `reply` overrides both the child `--me` and `--id` after the gateway checked a different, permitted original message. The probe uses two genuine AMQ request IDs in the separate sessions; it does not fabricate or alter message files.

```text
FLAGS: plain_cross_epic_send code=1 refused=true
FLAGS: send_hidden_override code=1 returned_error=true delivered_from=claude-02 delivered_to=[codex-02] provenance=false foreign_managed_drain_exposed=true
FLAGS: reply_hidden_override code=1 returned_error=true foreign_managed_drain_exposed=true
```

The error return is not rollback: both attempted messages were delivered and read. That remains a substantive supported-command policy defect and warrants fixing before relying on managed routing.

**Severity boundary.** `docs/design.md:1195`–`:1197` explicitly says this is a supported-path policy, not a sandbox: any same-UNIX-user process can access mailbox files and run raw AMQ with another `--me`. A caller able to invoke this local adapter already has those capabilities. The reproducer establishes an unintended policy bypass through the managed interface, not remote code execution, compromise of another UNIX principal, or a newly obtained host capability. Critical severity is unsupported; retain Medium for routing integrity and misleading failure semantics.

**Do not generalize the outbound proof to every inbound verb.** Two independent `read` attempts using a valued `--id --` to hide identity flags and a literal `--` delimiter did **not** expose the foreign body:

```text
FLAGS: read_attempt=1 code=1 returned_error=true foreign_body_exposed=false
FLAGS: read_attempt=2 code=2 returned_error=false foreign_body_exposed=false
```

This is not a proof that every inbound combination is safe. It shows why the broad traced conclusion needs a per-verb reproducer: `read` extracts its `--id` before forwarding (`managed.go:166`–`:173`), native read has no permitted non-routing string flag like send's `--subject` left to carry this marker, `thread` forces an admitted message-ID set (`:368`), `list` rechecks returned IDs against the caller's mailbox (`:426`–`:427`), and `trace` filters evidence after the child (`:316`). The native AMQ help for read/thread/dlq/list was inspected. No blanket inbound-disclosure claim is confirmed by this review.

**Fix direction:** Parse each managed verb once with flag arity matching its permitted surface, reject reserved/unknown or duplicate conflicting routing inputs, and construct child argv from typed parsed fields. Do not merely remove the `--` stop or move the existing unparsed remainder around: those preserve parser differentials. Keep the current raw-AMQ non-sandbox limitation explicit rather than claiming this change creates host isolation.

## SEC-2 — DOWNGRADE to Medium, with the policy claims separated

**Confirmed literal-argument defect (executed).** `internal/app/summon.go:324` appends a bootstrap to `r.Body`, and `startCommand` passes the resulting brief directly as a final argument for Codex (`:403`), Pi (`:407`), and Claude (`:409`) without an end-of-options delimiter. The real `Summon` plus fake terminal captured a dash-leading brief in exactly that slot:

```text
LAUNCH: case=1 action=start harness=codex parent_depth=1 child_depth=2 end_of_options=false prompt_starts_dash=true skip_permissions=false
```

An independent installed-Codex parser invocation with a literal brief beginning `--help=...` exited in argument parsing, demonstrating that this slot is interpreted as an option rather than a prompt:

```text
CODEX_PARSE: exit=2
error: unexpected value 'literal user brief
...
' for '--help' found; no more were expected
```

The invocation was `codex --no-daemon <one literal brief argument>`, matching the candidate's start argv shape. `CODEX_HOME` and adapter state were redirected to newly created scratch paths; Rex and harness session environment identifiers were removed. Exit 2 was checked, so no model turn or Rex adoption occurred. The standalone reproduction is `/tmp/rex-audit-20261112/probe-codex-01/cli-parser-probe.py` (the equivalent Python invocation was executed). This establishes a launch correctness defect with accepted input, not arbitrary shell splitting: the message body remains **one argv entry**, including spaces and bootstrap text.

`resumeCommand` also omits argument-boundary handling at `summon.go:420`–`:426`, but its automatically constructed resume prompt begins with `Resumed by ...` (`:278`); a dash-leading user-body claim does not directly apply to that fixed prompt. Hook session IDs normally come from the harness rather than an unauthenticated network sender. No cross-harness resume exploit was executed.

**Intentional Antigravity policy, not a newly verified escalation.** The captured qa launch really includes `--dangerously-skip-permissions` (`summon.go:405`, `:424`):

```text
LAUNCH: case=2 action=start harness=agy parent_depth=1 child_depth=2 end_of_options=false prompt_starts_dash=false skip_permissions=true
```

But `README.md:95`–`:96` and `internal/guide/skill.md:232`–`:234` explicitly prescribe it and explain why. Neither role names nor their prose briefings establish OS capability confinement. Introducing role-based execution permission or a new setup switch would change that declared product policy; the High report has not established a violated existing least-privilege contract.

**Grant/asked distinction.** `summon.go:309` skips the grant existence check when `Asked` is supplied, and the probe confirms a nonexistent grant plus `Asked` can start. The documented contract is a user's quoted words **or** a cited grant (`internal/guide/skill.md:292`–`:295`), with `--asked` explicitly self-attested; satisfying that alternative is not proof of bypassing it. Likewise depth 1 may summon depth 2 under `core/summon.go:58`; there is no documented caller-role authorization matrix to bypass. The fixture intentionally used a reviewer at depth 1 and recorded a depth-2 start.

There is a genuine smaller validation mismatch: a directory supplied with `--grant` alone passes `os.Stat` and launches, despite the flag being documented as a file:

```text
LAUNCH: case=3 action=start harness=codex parent_depth=1 child_depth=2 end_of_options=false prompt_starts_dash=false skip_permissions=false
```

Treat that as Low validation quality, not stronger authorization than the existing attestation model promises.

**Fix direction:** Encode literal prompt/session arguments according to each supported harness CLI's parser, with an end-of-options delimiter where supported and harness-specific tests for leading dashes. Validate a grant-only path as the documented readable regular file. Do not bundle unapproved per-role capabilities, changed Antigravity permission policy, or mandatory validation of an unused alternative grant into this bug fix.

## A11Y-1 — DOWNGRADE to Low; the new watchdog argument does not establish High impact

**Confirmed conditional interaction (executed).** `internal/screen/promptbox.go:215` rejects a titled edge lacking ten leading dashes; `core.TinyColumns` is 40 (`internal/core/watchdog.go:20`) and `MayMove` requires a strictly smaller dimension (`:35`–`:36`, `:59`–`:60`). Keeping a synthetic short-border input constant and passing widths into the real policy gives:

```text
WIDTH: cols=39 rows=20 synthetic_box=unknown stuck=true code=box_not_found may_move=true
WIDTH: cols=40 rows=20 synthetic_box=unknown stuck=true code=box_not_found may_move=false
WIDTH: cols=50 rows=20 synthetic_box=unknown stuck=true code=box_not_found may_move=false
WIDTH: cols=59 rows=20 synthetic_box=unknown stuck=true code=box_not_found may_move=false
WIDTH: cols=60 rows=20 synthetic_box=unknown stuck=true code=box_not_found may_move=false
```

This strengthens the statement **if the actual renderer produces that short-border shape**, a normal-height pane at widths 40 and above will not be automatically resized out of the classifier miss. The same synthetic shape misses even at 60 columns: the probe does not derive a 40–59 rendering band from the classifier. That band, and the claim that current Claude produces it from a 35–55-column title, remain assumptions without an authentic renderer capture/source trace.

**The failure is reported.** `internal/app/watchdog.go:184`–`:186` explicitly reports normal-size `box_not_found` to the orchestrator; it does not silently treat delivery as successful. In-band prompt/tool hooks remain a separate channel (`internal/app/hook.go:82`–`:89`), so a subsequent user turn or other activity can announce waiting mail. Doorbells can remain deferred indefinitely while the hypothetical layout and idle state persist, but “permanently stranded” is too broad, and High severity is unsupported without current-harness incidence or demonstrated unrecoverable mail loss.

The previous independent probe in `round2.go` also verified the ten-dash positive control, nine/one-dash false negatives, and drafted variants: every unknown variant refused a write. `internal/screen/screen_test.go:151`–`:153` intentionally rejects a similar short-rule/transcript shape. Raising the resize threshold or simply lowering the border prefix is not independently justified; obtain a current narrow Claude VT capture and add it alongside that negative safety control before changing recognition.

## Verification limits

- No production scope expansion: only candidate package calls, isolated AMQ roots, fake Rex, and a bounded argument-error-only Codex parser invocation. No real harness session, native permission dialog, or narrow Claude rendering was started.
- No remote threat actor or different-UNIX-user boundary was tested or found. The known raw-mail/filesystem non-goal remains relevant to security severity.
- Inbound parser combinations are not exhaustively proven safe; only the outbound exploit and the two reported inbound attempts above were executed. Avoid an unqualified all-verbs disclosure claim.
- One targeted Nowledge thread search was performed (`rex-amq-adapter audit managed mail takeFlag`, limit 3); no prior-thread content was used as evidence for these verdicts.
