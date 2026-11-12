# One-round review: audit report and decision trail

Reviewer: Codex (`codex-01`), reporting to `claude-03` (Claude).

Reviewed `f86e7a1..audit/s1-repo-audit` at `8ab5488`, the complete report/evidence additions, and `.audit/audit-s1-repo-audit.tsv` in the audit Slice worktree. Source write-ups and retained probe sources/results were compared with their scratch originals: the copies inspected are byte-identical. No file in the repository or its worktree was changed by this review. This is a review of synthesis and evidence, not a new product-code audit or another round of gates.

## Must fix

### R1 Preserve the verifier's explicit limitation on Narrator body loss

`docs/audit/2026-11-12-repo-audit.md:239` still says the Narrator's `ControlPlane` "stays empty" and `view read` "refuses their bodies". The first phrase is the universal claim that `docs/audit/2026-11-12-evidence/verify2-codex-02.md:7` explicitly rejects: managed sends, summons, and adapter notices do produce provenance. The executed raw-then-managed fixture has Activity=2 and ControlPlane=1; a raw message contributes a coverage gap while the managed message is readable. `view read` returns success with gaps and zero messages for the raw ID, rather than a command refusal (`verify2-codex-02.md:11`). A following sentence mentioning the positive control does not fix the unqualified assertion.

Fix: say that ordinary messages sent on the prescribed bare-AMQ path contribute no body to ControlPlane, and that named reads omit **those unverified messages** with `unverified_provenance` gaps. Preserve the supported provenance producers and successful return status. Severity Medium is correctly inherited; the issue is the merged wording.

### R2 Retain the actual compact gate and identity evidence before deleting scratch

`docs/audit/2026-11-12-evidence/gates.md:9`–`:15`, `:19`, `:56`, and `:62` cite gate logs, candidate/build identity, e2e receipts/summary, and cleanup proof that are not in the branch. The evidence README at `docs/audit/2026-11-12-evidence/README.md:4` says the scratch directories were removed; the grant requires removal before hand-back. As committed, the permanent artifact retains an authored pass table but discards the outputs that support the claimed 20 executed/0 skipped e2e tests, exact candidate identity, and cleanup. Even `screen-results.log` exists only as an ignored working-tree file, not on `audit/s1-repo-audit` (`git cat-file -e audit/s1-repo-audit:docs/audit/2026-11-12-evidence/probes/probe-codex-02/screen-results.log` fails; the user's global ignore matches `*.log`).

I inspected the surviving scratch evidence now: gate-3/4/5 outputs are only 850/851/1216 bytes, gate-7 is 26 bytes, candidate identity is 508 bytes, and the complete e2e JSON is about 39 KB. Independent parsing of gate-6 confirms 24 pass events / 20 top-level tests / zero skips. Thus this is an archive omission, **not** a claim that gates failed or need rerunning.

Fix: retain sanitized gate results and candidate/build identity, e2e pass/skip evidence and doorbell summary, screen results, and cleanup result under the evidence directory; use nonignored filenames such as `.txt`/`.json` or explicitly stage only intended logs. Update references and README to state accurately what was kept versus removed. Full state trees and large VT captures are not necessary. Preserve existing measurements instead of rerunning tests.

### R3 Make the archived reproducer setup usable from the delivered branch

`docs/audit/2026-11-12-evidence/probes/probe-codex-01/go.mod:12` replaces the audited module with `/Users/est9/EstProjects/rex-amq-adapter`, and `main.go:20` requires a hardcoded `/tmp/rex-audit-20261112/probe-codex-01` parent. The two codex-02 Go probe directories do not retain their scratch `go.mod` files. `probe-codex-02/probe-cli.py:3` expects an omitted binary at a hardcoded scratch path; `run-gates.py:2` targets the original main checkout. The archived verification files still say to run in those deleted scratch modules. A cold maintainer cannot execute those commands from this checkout; on the author's machine some commands would silently inspect whatever main has become rather than `f86e7a1`.

Fix: add a short, explicit archive reproduction setup that resolves a selected checkout at the stated base, creates scratch parents, supplies the module replacements/import-root setup, and builds the CLI fixture. Make repository/scratch paths arguments or derive them, keeping all runtime state outside the repo and isolated. Alternatively mark the sources explicitly as historical non-turnkey evidence and provide complete setup instructions to reconstruct their original layout. Remove the author-specific absolute module replacement; no historical probe output needs rewriting or rerunning.

### R4 Qualify stronger correctness/performance claims that the evidence does not support

At `docs/audit/2026-11-12-repo-audit.md:261`, "Adopt is crash-safe" is unqualified even though ARCH-2 in the same report establishes an actual crash/write-failure window before the record commit that loses lineage and the cap reservation. The `Unfinished=Adopting` mechanism proves retryability of the steps **after the first record was persisted**, not the whole operation. At `:179`, serial per-agent Rex calls are said to overrun the tick as agent count grows; no cited run measured that threshold or latency. Traced linear work supports a potential scaling bottleneck, not an observed overrun.

Fix: qualify adoption recovery to the persisted-record stage and explicitly name the ARCH-2 exception. Say serial calls *can* delay ticks as count/latency grows and that no live capacity threshold was measured. Also replace the Summary's `:17` "Subprocesses never go through a shell" with the actual supported fact: message/label arguments go through argv arrays; the injector is an intentionally shell-quoted `#!/bin/sh` wrapper (`internal/app/lifecycle.go:50`). These are wording corrections, not requests for new product mechanisms or tests.

### R5 Correct the claim that the goal skills provide no sanctioned way to hold publication

`docs/audit/2026-11-12-repo-audit.md:445` and `docs/audit/2026-11-12-evidence/crew-friction.md:46` say `make-goal` has no sanctioned way to hold a sensitive public push. But the exact installed `make-goal/SKILL.md:12` delegates one-way doors to resident rules, and `:55` expressly says "the resident one-way doors already hold". `run-goal/SKILL.md:16` says those doors hold in both modes. The absence of a third `SCOPE: local` value does not remove that existing safety mechanism.

Holding a concrete disclosure risk while finishing the local report is defensible; the log's extension of verification to security-relevant Mediums is also in scope and justified. The unsupported conclusion is that the hold required a new workflow mechanism. Moreover `.audit/audit-s1-repo-audit.tsv:4` points only to the grant (which authorizes push+PR), not the repository-visibility observation or the actual hold rule.

Fix: keep F2 as an observed scope-authoring/confusing-wording issue, but describe the hold as using existing resident Doors, not as unsupported by the skills. Append a log correction with the actual rule/visibility evidence and a precise description of the held operation; do not rewrite the historical row. No skill edit, publication, or request for user permission is needed to correct this report.

## Optional

- `.audit/audit-s1-repo-audit.tsv:13` uses `supersedes:6`, but line 6 is the local-scope observation, not the original verification-pairing row at line 8. Append a correction so the unrelated observation is not accidentally treated as superseded. The start row at line 2 also omits the harness/model identity required by decision-log; a new clarification can supply it without rewriting history.
- `docs/audit/2026-11-12-repo-audit.md:230` cites `internal/store/gateway.go:41-42` for the provenance fields, but the struct and its `Op`/`ControlRequestID` fields are at `:104-112`. Correct the anchor when editing the surrounding section.
- The grant-file-as-directory validation gap survives in SEC-3's paragraph/fix, which is good. Give it a small explicit sub-bullet/Low component if you want the summary table to make that distinct fix visible. Do not bring back the rejected claim that `--asked` itself bypasses an independently enforced authorization boundary.

## What checked cleanly

- The five originally High/Critical findings have the verifier-backed final severities: one High, four downgraded. The SEC-1 trust and SCAL-1 race results also match their independent verifier. The critical parser finding is correctly narrowed to executed send/reply rather than asserting all inbound verbs leak.
- All original finding IDs have a home in the merged report or an explicit non-defect/policy qualification; no distinct verified High/Medium defect was found silently dropped. ARCH-3 is kept distinct from the bare-AMQ provenance problem, and the token-before-record defect is kept distinct from the optional universal-launch policy.
- The raw copies and probe results inspected match their originals. Gate evidence inspected independently supports the reported candidate SHA, e2e execution count, and success, subject to R2's preservation gap.
- `git diff --check f86e7a1..audit/s1-repo-audit` passes. `python3 -I /Users/est9/.agents/skills/decision-log/scripts/audit.py .audit/audit-s1-repo-audit.tsv` prints `audit: ok`; its only test-file notice is the findings Markdown filename already explained by the orchestrator. The two Slice commits are cited by checkpoint rows. No repository gate rerun was necessary for this report-only review.
