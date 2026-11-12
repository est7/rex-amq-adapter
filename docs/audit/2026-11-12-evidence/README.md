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
