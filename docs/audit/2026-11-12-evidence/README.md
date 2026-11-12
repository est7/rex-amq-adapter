# Evidence for the 2026-11-12 audit

Raw round-1 and round-2 write-ups from each auditor, the gate log, and the probe sources. The
write-ups cite paths under `/tmp/rex-audit-20261112/`; those scratch directories were removed after
the audit. `/tmp/rex-audit-20261112/probe-<agent>/X` is kept here as `probes/probe-<agent>/X` (sources and
small result files only; generated state and large logs were not kept).

| File | Author | Content |
|---|---|---|
| sec-perf-scal.md | codex-01 | round 1: security, performance, scalability |
| arch-maint-doc.md | claude-04 | round 1: architecture, maintainability, documentation |
| test-auto-a11y.md | codex-02 | round 1: testing, automation, accessibility |
| agy-all.md | agy-01 | round 1: all nine dimensions, blind to the others |
| gates.md | codex-02 | gate commands, exit codes, durations, coverage |
| verify-claude-04.md, verify-codex-01.md | | round 2 on Medium findings |
| verify2-codex-01.md, verify2-codex-02.md | | round 2 on agy-01's High/Critical findings |
| crew-friction.md | orchestrator | friction in the crew/goal path observed during the run |

## Re-running a probe

The Go probes are separate modules that import the adapter through a `replace` directive pointing at
the auditor's checkout (`probes/probe-codex-01/go.mod:12`). To re-run one:

```sh
git worktree add /tmp/audit-base f86e7a195b3442c381434220f737b8d0561e61f5
cp -R docs/audit/2026-11-12-evidence/probes/probe-codex-01 /tmp/probe && cd /tmp/probe
go mod edit -replace github.com/est7/rex-amq-adapter=/tmp/audit-base
REX_AMQ_ADAPTER_HOME=$(mktemp -d) go run -mod=mod . [hook|profile|round2|round2b]
```

The same applies to `probe-codex-02/screen-probe` and `probe-codex-02/round2b`. Scripts that drive
the CLI (`probe-cli.py`, `cli-parser-probe.py`) take a built candidate binary: `go build -o
/tmp/rex-amq-adapter ./cmd/rex-amq-adapter` in the base worktree, and pass or edit the binary path
at the top of each script. Every probe needs `REX_AMQ_ADAPTER_HOME` set to a scratch directory; never
run a candidate build against the real state directory.

`probe-codex-02/gate-6-summary.tsv` condenses the e2e JSON log (24 pass events, 20 top-level, 0
skips); the full 39 KB JSON log was not kept.
