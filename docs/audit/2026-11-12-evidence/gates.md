# Audit gates

Candidate: main f86e7a195b3442c381434220f737b8d0561e61f5; tracked diff SHA-256 e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855. Untracked `.goal/` belongs to orchestrator and is excluded. Environment: go version go1.27.4 darwin/arm64; ProductName:		macOS; ProductVersion:		27.3; BuildVersion:		26C5035r; ; 0.141.0 amq; rex 0.0.1793981048-g01d6c4; commit: 01d6c4fe68668db3521570d7fbc9b6b112b1e572; built: 2026-11-06T16:04:08Z; api: revision 11 (supports 0 to 11); .

Plan: build must compile; vet must report no diagnostics; short tests and race tests must pass; coverage must identify package/function gaps; e2e must actually run private Rex/amq delivery scenarios with no skipped tests; vulnerability scan must complete and report no reachable vulnerabilities. Run suites serially, record first failures, and never retry e2e. E2e uses `-json` solely to retain executed/skipped test evidence.

| Check | Status | Exit | Seconds | Command | Log |
|---|---|---:|---:|---|---|
| 1 | Passed | 0 | 0.67 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go build ./...` | /tmp/rex-audit-20261112/probe-codex-02/gate-1.log |
| 2 | Passed | 0 | 0.14 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go vet ./...` | /tmp/rex-audit-20261112/probe-codex-02/gate-2.log |
| 3 | Passed | 0 | 175.93 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go test -short ./...` | /tmp/rex-audit-20261112/probe-codex-02/gate-3.log |
| 4 | Passed | 0 | 185.38 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go test -short -race ./...` | /tmp/rex-audit-20261112/probe-codex-02/gate-4.log |
| 5 | Passed | 0 | 177.69 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go test -short -coverprofile=/tmp/rex-audit-20261112/cover.out ./...` | /tmp/rex-audit-20261112/probe-codex-02/gate-5.log |
| 6 | Passed | 0 | 11.75 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go test -json ./internal/e2e/...` | /tmp/rex-audit-20261112/probe-codex-02/gate-6.log |
| 7 | Passed | 0 | 10.94 | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) go run golang.org/x/vuln/cmd/govulncheck@latest ./...` | /tmp/rex-audit-20261112/probe-codex-02/gate-7.log |

## Executed e2e evidence

20 top-level tests and 24 total test pass events; no failures, skips, or cached result. The harness built `$TMPDIR/rae-adapter-64277` directly from this checkout and assigned its hooks to the private scripted agents (`internal/e2e/harness_test.go:105-153`). A concurrent read-only collector matched each private Rex server by parent e2e-test pid and retained VT captures, sessions/blocks JSON, hooks and `.got` files under `probe-codex-02/e2e-surface/`. The collector observed 20 private servers and saved 21 VT snapshots. Only one snapshot contains the rendered scripted-agent input box; the others show startup/shell state. Separate `.got` files retain 12 actual doorbell receptions, so no claim is made that every test has a final rendered screen snapshot. `probe-codex-02/e2e-summary.json` lists 12 captured doorbell-receipt files. It never addressed the live Rex server. No e2e retry was run.

## Per-package coverage

| Package | Coverage |
|---|---:|
| cmd/rex-amq-adapter | 20.2% |
| internal/amq | 54.2% |
| internal/app | 82.0% |
| internal/core | 83.6% |
| internal/crew | 89.8% |
| internal/fsutil | 0.0% |
| internal/install | 87.7% |
| internal/rex | 17.9% |
| internal/screen | 89.9% |
| internal/store | 51.9% |
| internal/transcript | 82.6% |
| internal/trust | 82.2% |

Total statements: **72.4%**. Function details: `probe-codex-02/coverage-functions.log`. This is default package-local instrumentation; functions exercised only as dependencies or in subprocess-built binaries can show 0%. It is not an integration-coverage percentage, and the e2e run did not use coverage instrumentation.

## Additional probe matrix

| Probe | Result | Evidence |
|---|---|---|
| Root help | Passed | `cli-results.json`: usage, exit 0 |
| `title --help` is read-only help | Failed | stored task changes to `--help`, rename command invoked, exit 0 |
| `crews --help` is command help | Failed | crew table instead of usage |
| `crews --json` is JSON or explicit unsupported-flag error | Failed | text table, exit 0 |
| `who --json` | Blocked | unsupported by current CLI; exit 2 |
| Standard CJK/rocket task truncation | Passed | `screen-results.log`: 17 double-width characters become 15 plus ellipsis |
| Narrow titled Claude empty box recognized | Failed | `screen-results.log`: unknown; wide control is empty |
| Live current-model narrow Claude rendering | Blocked | live-pane/model operations outside read-only grant |
| `REX_AMQ_LIVE=1` client suite | Skipped | it creates a tab on the human live server; outside grant |

## Candidate and build identity

`probe-codex-02/candidate.json`, `binary-buildinfo.txt` and `cli-results.json` retain OS/tool versions, SHA and binary SHA-256. `go version -m` confirms `vcs.revision=f86e7a195b3442c381434220f737b8d0561e61f5`; `vcs.modified=true` is explained by the pre-existing untracked `.goal/`, while the tracked diff is empty. Final `git rev-parse HEAD` and tracked diff hash match the start (`final-identity.txt`).

## Collection notes

A first evidence-collector launch matched its own shell command text, then exited without capturing anything; its process detection was corrected before e2e began. A second pre-e2e collector was stopped and relaunched to remove inherited Rex session scoping and require a visible prompt in VT captures. These were collector setup corrections, not test retries; all gate first results are retained above.

Cleanup Passed: `probe-codex-02/cleanup-check.json` records no remaining private test servers, e2e-test process, candidate injector/watcher or private waker matching this run. `$TMPDIR/rae-adapter-64277` was removed by TestMain. A separate process inspection found only the four existing live-session wakers; none was stopped or changed. The scratch-built CLI binary and evidence files remain for audit; no scratch server/process remains running.
