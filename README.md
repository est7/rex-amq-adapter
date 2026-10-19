# rex-amq-adapter

Coding agents running in [Rex](https://www.superlogical.com/rex/docs) terminals
message each other over [AMQ](https://github.com/avivsinai/agent-message-queue).
Install it once, and every Claude Code, Codex, Antigravity (`agy`), or Pi
session you start in a Rex terminal gets a mailbox under its pane's label (`claude`, `codex-2`, or a name you
choose) and is told when mail arrives. No `amq init`, no environment variables,
no daemon.

```
claude pane ── amq send --to codex ──▶ shared AMQ root ──▶ amq wake (codex's waker)
                                                             │ --inject-via
                                                             ▼
                                   rex-amq-adapter inject … (codex is idle)
                                                             │ rex block call write
                                                             ▼
                                           codex: "AMQ doorbell … run amq drain"

codex busy in a turn? its own hooks deliver instead:
  PostToolUse → notice added to its context      Stop → turn continues with the notice
```

## How delivery works

The agent's own status report (Rex's OSC 7501 program status, which Claude Code,
Codex, and Antigravity emit) chooses the channel:

| Agent is | Mail is announced by | Evidence it arrived |
|---|---|---|
| working | its own hooks, inside the turn (`PostToolUse` / `UserPromptSubmit` / `Stop`; Antigravity: `PreInvocation` / `Stop`) | the notice is in the model's context |
| idle at its prompt, input box empty | a doorbell typed by the injector | the agent reports `working` |
| idle, but someone is typing in its box | nothing yet; amq retries (5 s doubling to 2 min) | — |
| waiting on a question or permission dialog | nothing yet | — |
| Pi, in any state | its extension, in-process: a turn of its own when idle, steered into a running turn | the notice is in the model's context; nothing is typed |

Text is typed only into an agent at rest with an empty input box, as one
bracketed paste (Codex otherwise swallows the Enter). A doorbell left unsubmitted
is erased only when the box holds exactly that doorbell. See
[docs/design.md](docs/design.md) for the contract and the probes behind it.

## Install

Requirements: Rex (macOS), `amq` on `PATH` (tested with 0.98.0; needs
`amq wake check --json-schema 2` and `retire --if-generation`, as in 0.81.2+),
Go to build, and Claude Code (2.1.303), Codex (0.163.0), Antigravity CLI
(1.4.2), or Pi (0.84.2) — the versions tested.

```bash
go install github.com/est7/rex-amq-adapter/cmd/rex-amq-adapter@latest   # or, in a checkout:
# go build -o ~/.local/bin/rex-amq-adapter ./cmd/rex-amq-adapter
rex-amq-adapter setup        # skill + hooks for every agent found; --dry-run to preview
```

The hooks run the binary at the absolute path `setup` was run from.

`setup` appends five hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`,
`Stop`, `SessionEnd`) to `~/.claude/settings.json` and `~/.codex/hooks.json`,
leaving every other entry, key, and position as it was; adds a hook group named
`rex-amq-adapter` (`SessionStart`, `PreInvocation`, `Stop`) to Antigravity's
`~/.gemini/config/hooks.json`; installs a small JavaScript extension for Pi in
`~/.pi/agent/extensions/`; and installs a skill stub whose guide ships inside
the binary. Run it again after moving the binary.

- **Codex** runs new hooks only after you approve them: run `/hooks` in Codex, or
  accept the review prompt at startup. The first hook that runs adopts the agent.
- **Claude Code** and **Antigravity** run hooks only in folders you trust.
- An agent gets its mailbox when its first turn starts; Codex and Antigravity
  start none on their own, so give a peer its brief as the first prompt.

`rex-amq-adapter status` reports each item as `current`, `outdated`, `missing`, or
`foreign`; `rex-amq-adapter remove` deletes only what setup installed.

## Use

Start agents in Rex terminals as usual. Then ask one to talk to another:

> ask codex to review the current diff over AMQ

The agent's skill handles `amq send`, `amq drain`, and `amq reply`, and opens
peers itself: a pane beside it for the same task, a new tab for another task in
the same project, a new session for another project. To start one by hand,
label its pane (the label becomes the handle) and give it a first prompt (an
agent gets its mailbox when its first turn starts):

```bash
rex split --split=right --focus=false --label reviewer -- codex "Review the diff; report to claude over AMQ"
```

Inspect state:

```bash
rex-amq-adapter who          # handles, kind, live/departed, unread, last doorbell, Rex block
eval "$(rex-amq-adapter env)" && amq who --json    # inside an adopted pane
rex-amq-adapter collect      # retire agents whose pane closed or process died
```

## Lifecycle

| Moment | What happens |
|---|---|
| Agent's first hook runs | Handle chosen (pane label, else `<kind>`/`<kind>-N`), mailbox created, `amq wake` started, pane relabeled to the handle |
| Agent exits | `SessionEnd` retires its waker; the handle stays reserved for 24 h |
| Agent crashes or its pane closes | The next `SessionStart` (or `collect`) notices and does the same |
| Same pane, new agent | Gets its previous handle back |

## State

`~/.local/state/rex-amq-adapter/` (override with `REX_AMQ_ADAPTER_HOME`):
`amq-root/` (the shared AMQ root), `records/` (one per adopted pane),
`ledger/` (announced message ids), `inject/<handle>.jsonl` (every doorbell
attempt and why it waited, trimmed past 1 MB; `who` shows the latest as LAST
DOORBELL, e.g. `deferred box_not_found 23m` when the adapter cannot find an
agent's input box), `logs/<handle>.log` (amq wake), `logs/hooks.log`
(hook failures; hooks run with stderr discarded).

## Development

```bash
go test ./...                              # one test drives the real amq binary in a temp root
REX_AMQ_LIVE=1 go test ./internal/rex/     # inside a Rex terminal: the rex CLI client against the live server
```

`internal/core` holds all policy and has no I/O; `internal/app` wires it to the
`rex` CLI (`internal/rex`), `amq` (`internal/amq`), and the state directory
(`internal/store`). `internal/screen` is the input-box classifier ported from
[herdr-amq-adapter](https://github.com/est7/herdr-amq-adapter).

## Known gaps

- OpenCode is not supported.
- A departed handle's mailbox is kept; nothing archives old mailboxes yet.
- Cross-machine delivery is out of scope.
- An agent that ends a turn with mail it chose not to drain gets its next
  doorbell on amq's retry ladder, up to 2 minutes later.

## License

MIT. `internal/screen` is ported from herdr-projects (MIT); see `internal/screen/testdata/NOTICE.md`.
