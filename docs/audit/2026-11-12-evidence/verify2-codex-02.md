# Round 2b independent verification: agy-01 ARCH-1 and ARCH-2

Verifier: codex-02. Candidate remains `main@f86e7a195b3442c381434220f737b8d0561e61f5`; tracked diff SHA-256 remains `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`. Only pre-existing `.goal/` is untracked. No repository source, tests, installation or live Rex/AMQ state changed.

## ARCH-1 — DOWNGRADE to Medium

The recommended bare-amq flow does deprive Narrator of ordinary agent-authored message bodies and can accept a cross-Epic send that is later quarantined without a sender bounce, but “provenance is never written / ControlPlane is permanently empty / every body read is refused” is too broad: managed sends, summons and adapter notices produce provenance, and the executed managed-send positive control populates ControlPlane and is readable.

- Evidence level: executed for view/body behavior and sender bounce; traced for other production provenance producers.
- Instructions really prescribe bare mail: `internal/guide/skill.md:11`, `:82`, `:114`, `:123`; `internal/crew/builtin/roles/narrator.md:4-6` also prescribes bare drain/reply. Bare sends do not enter `app.post`, so no adapter provenance is written for that send.
- Body gating is explicit: `internal/app/view.go:382-391` marks unrecorded obligations unverified; `:419-430` records an unverified coverage gap and omits their bodies from ControlPlane; `:534-543` excludes them from `view read`. The API succeeds with `unverified_provenance` gaps and zero returned messages rather than returning a command error. Activity/envelopes and open obligations remain visible; this is loss of body-backed observation, not a completely absent session view.
- The universal claim is disproven by `internal/app/gateway.go:171-203`, which persists pending/committed provenance; `:229-237` routes internal notices and summon mail through that same producer. `internal/app/view.go:289-296` includes any recorded message sent or received by an orchestrator, not only `OpControl` messages. Thus ARCH-2 does not imply that the view's differently named ControlPlane field is unreachable.
- Isolated raw same-Epic send: provenance absent; Activity count 1; ControlPlane count 0; `ReadSessionMessages` returned zero bodies and `unverified_provenance`. A subsequent managed send from the same orchestrator to the same worker produced provenance, raised Activity count to 2 and ControlPlane count to 1, and returned one body through `ReadSessionMessages`. Both used real amq 0.141.0 and the exact repository code; Rex session metadata came from a fake CLI.
- Cross-Epic raw send returned a nonempty ID with no error; the recipient's managed drain returned exit 0 / `count: 0`, with `QuarantineReason(...).Decision.Reason == cross_epic`. This follows `internal/app/gateway.go:47-57,100-104`.
- The sender's real `Hook(EventPostTool, Pi, ...)` then returned an empty Notice while advancing its sent cursor to that exact ID. The recipient was still live. `internal/app/undelivered.go:193-206` excludes live recipients and only looks for unread copies left in departed inboxes; it does not look for quarantine outcomes. Therefore the quiet sender result is confirmed, not inferred from a missing CLI error alone.
- Severity rationale: the recommended communication path is misaligned with a supported observation feature and gives poor delivery feedback, but ordinary within-Epic mail still works, unverified bodies are explicitly reported as unknown, and the managed alternative works. These additions do not establish a High-severity total loss or security escape. Keep this merged with the already confirmed bare-amq/managed-path Medium finding. This assignment did not independently re-run the separate TOCTOU/security-bypass claims.
- Fix direction: make the prescribed commands use managed verbs and surface recipient quarantine to the sender. Do not “fix” this by guessing provenance or allowing Narrator to read unverified bodies.

## ARCH-2 — CONFIRMED at High

The sole documented Narrator-to-Orchestrator user-control path has no supported producer: production managed sends/replies construct only OpSend/OpReply, raw sends are treated as OpSend, and no production caller creates OpControl plus a request ID; both exercised delivery paths reject the directive even though the pure policy admits a correctly constructed control operation.

- Evidence level: executed for real managed/raw failure plus policy positive control; traced for absence of a production producer.
- Contract: `internal/crew/builtin/roles/narrator.md:7` instructs verbatim forwarding of a personal agent's user-control directive with its request ID. `docs/design.md:1170-1173` makes this the narrator's one message into the Epic. No recommendation to originate arbitrary technical work is involved.
- Necessary condition: `internal/core/gateway.go:147-151` permits Narrator -> Orchestrator only with `op.Kind == OpControl` and nonempty `op.ControlRequestID`.
- No construction path: `internal/app/managed.go:91-109` creates `OpSend`, changing only to `OpReply` for replies; arbitrary message `--kind` or `--context` values do not set `Outgoing.Op`. `internal/app/gateway.go:47-51` treats untokenized raw mail as `OpSend`; `:229-237` uses `OpSend` for notices and summon mail. Repository-wide `rg` of non-test `cmd/` and `internal/` finds `OpControl` only in its definition and policy comparison; ControlRequestID only in its type/persistence/reconstruction fields, not a producer.
- Pure-policy positive control with same-session Narrator and Orchestrator, `Operation{Kind: OpControl, ControlRequestID: "req-1"}` returns `{allow:true, reason:narrator_control}`; omitting the ID returns the routine-mail denial.
- Actual managed send with valid `--kind todo`, body `stop now`, and `--context '{"control_request_id":"req-1","op":"control"}'` returns exit 1 and `refused by the gateway: narr to orch (narrator_sends_orchestrator_control_only)`. Context content cannot convert it into the required operation.
- Actual raw Narrator send succeeds at amq, but the orchestrator's managed arrival/drain quarantines it with `narrator_sends_orchestrator_control_only`. Nothing in the recommended raw path supplies a trustworthy control operation either.
- Severity rationale: a valid stop/change/decision cannot traverse this feature's only permitted write channel at all; switching instructions from raw to managed does not repair it. This is complete loss of a documented control capability, distinct from ARCH-1's missing provenance for ordinary messages. High is justified for that scoped feature; it does not mean ordinary worker/orchestrator messaging or the entire adapter is unavailable. A human directly contacting the orchestrator is an out-of-band workaround, not the promised relay path.
- Fix direction: add an explicit managed control operation that preserves the originating request ID and role/link policy, constructs `Outgoing.Op` authoritatively, and validates arrival/read reconstruction. Merely passing a message header label or JSON context key must not grant control semantics.

## Probe plan and result matrix

Probe source: `/tmp/rex-audit-20261112/probe-codex-02/round2b/main.go`. Scratch module uses a local replace directive to import the candidate's exact internal packages. All mailbox and adapter writes are under a fresh `round2b/state-*` root. All Rex calls use a fake shell binary with a nonexistent isolated socket URI. No wakers, server, emulator or background processes were started.

Run in that scratch module:

```sh
eval "$(/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter env)" && REX_AMQ_ADAPTER_HOME=$(mktemp -d /tmp/rex-audit-20261112/probe-codex-02/round2b/env-XXXXXX) go run -mod=mod . > run.log 2>&1
```

The installed env command only loads this verifier's identity; every probe mail operation explicitly targets the scratch AMQ root, and `internal/amq` removes inherited AMQ routing settings before executing real amq.

| Check | Result | Oracle |
|---|---|---|
| Raw same-Epic provenance/body exclusion | Passed | No provenance, no ControlPlane body, read reports unverified |
| Managed same-Epic positive control | Passed | Provenance exists, ControlPlane has exactly its body, named read returns it |
| Cross-Epic raw send and arrival quarantine | Passed | Send ID returned; quarantine reason cross_epic; drain has no body |
| Sender notification after quarantine | Passed | Post-tool hook Notice empty; sent cursor advanced to the cross-Epic ID |
| Valid pure control policy | Passed | OpControl plus req-1 allowed; missing ID denied |
| Managed Narrator directive | Passed | Expected failure exit 1 / narrator_sends_orchestrator_control_only |
| Raw Narrator directive | Passed | Send succeeds, then expected arrival quarantine with same denial |
| Live Rex/current agents | Skipped | Explicitly outside the isolated-probe brief |

Raw evidence: `round2b/results.json` contains full view/read outputs, quarantine records, policy decisions and sender hook/cursor state. `round2b/run.log` retains diagnostics. The final probe exits 0 and panics if any listed observation is false.

First failure retained: `round2b/first-run.log` and `first-run-source.txt`. Its cross-Epic sender-notice assertion encountered two unrelated unread messages left over from the preceding view scenarios. The fixture was corrected by draining those earlier messages through the actual managed API before the cross-Epic scenario; the original empty-notice assertion remained unchanged. This was probe scenario isolation, not a product fix or an edited repository test. A later summary-print helper initially assumed non-null empty ControlPlane/message slices; its display-only null handling was corrected without re-running the product probe.

The full gates from Round 1 remain applicable: source identity is unchanged. No full suite or e2e rerun was necessary for this read-only verification.
