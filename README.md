# rex-amq-adapter

Coding agents running in [Rex](https://www.superlogical.com/rex/docs) terminals
message each other over [AMQ](https://github.com/avivsinai/agent-message-queue).
Install it once, and every Claude Code, Codex, Antigravity (`agy`), or Pi
session you start in a Rex terminal gets a mailbox under a handle of its kind,
role, and a number (`codex-reviewer-01`, or `claude-01` when the pane has no
role), keeps it when you resume the session in another pane, and is told when
mail arrives. No `amq init`, no environment variables,
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

Requirements: Rex (macOS), `amq` on `PATH` (tested with 0.123.0; needs
`amq wake check --json-schema 2` and `retire --if-generation`, as in 0.81.2+),
Go to build, and Claude Code (2.1.316), Codex (0.166.0), Antigravity CLI
(1.4.2), or Pi (1.4.0) — the versions tested.

```bash
brew install est7/tap/rex-amq-adapter      # macOS / Linux; pulls in amq
rex-amq-adapter setup                       # skill + hooks for every agent found; --dry-run to preview
```

Or with Go: `go install github.com/est7/rex-amq-adapter/cmd/rex-amq-adapter@latest`
(install `amq` yourself). Under Homebrew the hooks run the stable
`$(brew --prefix)/opt/rex-amq-adapter/bin` path and the agent guide ships in
the binary, so `brew upgrade` keeps both current. The Pi extension is a copy:
when `rex-amq-adapter status` reports an item `outdated` after an upgrade, run
`setup` again. Wakers run a small wrapper in the state directory
(`bin/inject`) that execs the binary: amq refuses an `--inject-via` under a
group-writable directory such as Homebrew's Cellar, and the wrapper's path
survives the binary moving.

Run one version for every hook: an adapter older than v0.6.0 reads a pane
label's task (`claude-goal-01 | fix login`) as part of the role, so a new agent
it names in that pane gets a handle like `claude-goal-01-fix-login-01` and
loses its role. Before downgrading, clear each agent's task with
`rex-amq-adapter title`; an agent already named that way needs a fresh start in
a pane labelled with the role alone.

The hooks run the binary at the absolute path `setup` was run from.

`setup` appends five hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`,
`Stop`, `SessionEnd`) to `~/.claude/settings.json` and `~/.codex/hooks.json`,
leaving every other entry, key, and position as it was; adds a hook group named
`rex-amq-adapter` (`SessionStart`, `PreInvocation`, `Stop`) to Antigravity's
`~/.gemini/config/hooks.json`; installs a small JavaScript extension for Pi in
`~/.pi/agent/extensions/`; and installs a skill stub whose guide ships inside
the binary. Run it again after moving the binary.

- **Codex** must be started with `codex --no-daemon` (resume:
  `codex --no-daemon resume <session-id>`). The shared background server can
  lack the pane's Rex environment even when the frontend runs inside Rex.
  Pass the flag explicitly when launching through Rex; fish abbreviations do
  not apply there. This mode cannot be combined with `--remote` or `codex agents`.
  Codex runs new hooks only after you approve them: run `/hooks` in Codex, or
  accept the review prompt at startup. The first hook that runs adopts the agent.
- **Claude Code** and **Antigravity** run hooks only in folders you trust.
- **Antigravity** peers start with `agy --dangerously-skip-permissions -i "<brief>"`;
  without the skip flag agy stops to ask before every `amq` command it runs.
- An agent gets its mailbox when its first turn starts; Codex and Antigravity
  start none on their own, so give a peer its brief as the first prompt.

`rex-amq-adapter status` reports each item as `current`, `outdated`, `missing`, or
`foreign`; `rex-amq-adapter remove` deletes only what setup installed.

## Use

Start agents in Rex terminals as usual. Then ask one to talk to another:

> ask codex to review the current diff over AMQ

Agents in one Rex tab know each other: an agent learns who is beside it when it
starts, and the agents already there hear that it joined, or later left,
with their next prompt. "Ask the one beside you" needs no handle.
Other tabs and sessions are outside that awareness boundary: their agents
do not appear in discovery or join/leave notices.

A pane label names the agent's role, and a role with a prompt briefs the
agent when it starts. A tab named after a crew adds the crew's team
conventions, shared by every agent in it:

| Built in | What it is |
|---|---|
| roles `implementer`, `reviewer`, `qa`, `architect`, `writer`, `editor` | work in any tab |
| role `goal` | owns a goal and runs it unattended (below) |
| crew `pair` | implementer builds, reviewer critiques the working-tree diff, until clean |
| crew `ship` | `pair`, then QA runs the reviewed candidate and reports a verdict with evidence |
| crew `dev` | an architect decomposes the goal into tasks for an implementer and reviewer |
| crew `docs` | writers draft, an editor checks against the code; an optional architect partitions |
| crew `design` | architects propose independently, critique each other, and report a decision or the crux |

```bash
rex-amq-adapter tab pair --role implementer -- codex --no-daemon "Fix #42: …"
rex split -b <its block> --label reviewer -- claude "Review the implementer's fix for #42 when it hands off."
rex-amq-adapter crews        # every crew and role available in this directory
```

A tab without a `goal` agent assumes you are watching: an agent that stops is
yours to notice. Label a pane `goal` and the tab runs unattended instead. That
agent owns the goal and decides how to recover: it is woken at once when a peer
quits, crashes, or has its pane closed, with the session to resume, and it
restarts or replaces the peer itself. If the goal agent is the one that goes, one
peer is told to report and stop; with no peer left, nobody is told. A small watcher process runs for each goal
agent and ends with it.

```bash
rex-amq-adapter tab pair --role goal -- claude "Ship #42. Done when CI is green. You may open a PR but not merge. Stop after two failed attempts."
```

Your own crews and roles override the built-in ones by name: a crew is a
directory holding `team-conventions.md` and, optionally, `<role>.md` for a
role that works differently in it; `roles/<role>.md` holds roles for any tab.
They are read from `.agents/crews/` in the project (or a parent directory),
then `~/.config/rex-amq-adapter/crews/`. The format follows
[Runner](https://github.com/yicheng47/runner)'s crew examples, which the
built-in prompts are rewritten from.

The agent's skill handles `amq send`, `amq drain`, and `amq reply`, and opens
peers itself: a pane beside it for the same task, a new tab for another task in
the same project, a new session for another project. To start one by hand,
label its pane (the label becomes the handle) and give it a first prompt (an
agent gets its mailbox when its first turn starts):

```bash
rex split --split=right --focus=false --label reviewer -- codex --no-daemon "Review the diff; report to claude over AMQ"
```

Inspect state:

```bash
rex-amq-adapter who          # running agents in this Rex tab, including yourself
rex-amq-adapter who --all    # also this tab's departed agents reserved for a resume
rex-amq-adapter await <block> # wait for an agent you started; prints its outcome as JSON
rex-amq-adapter collect      # retire agents whose pane closed or process died
```

An agent shows what it is working on after its handle in its pane's label
(`claude-reviewer-01 | fix login redirect`) with `rex-amq-adapter title
"<task>"`; its identity notice tells it to. The task follows the handle into
any pane it is resumed in.

`who` resolves the caller's current tab from `REX_BLOCK`; it requires a Rex
terminal and fails if the tab cannot be resolved. It uses the current layout,
so moving a block changes which tab discovers it. `--all` includes departed
agents last recorded in this tab, never agents from other tabs. A failure
while reconciling the tab is a warning on stderr; the tab is still listed.
`who` also brings back a dead waker in the tab, so an idle agent hears its mail
again once a peer checks on it, and records where moved agents are now. Use the adapter's `who`, not the shared mailbox
directory from `amq who`.

## Lifecycle

| Moment | What happens |
|---|---|
| Agent's first hook runs | Handle chosen (`<kind>-<role>-<NN>`, the role from the pane label, else `<kind>-<NN>`), mailbox created, `amq wake` started, pane relabeled to the handle (then `<handle> \| <task>` once the agent names its task); it is told its window-mates and its crew and role briefing, and its window-mates are told it joined |
| Agent exits | `SessionEnd` retires its waker and tells its window-mates it left; the handle stays reserved for its session for 7 days |
| Agent crashes or its pane closes | The next adoption or `collect` notices and does the same, as does `who` in that tab, a send to it, or within seconds a `goal` agent there |
| Session resumed (`claude --resume`, `codex resume`), in any pane | Gets its handle back, with the mail that arrived meanwhile; so does a Claude conversation that moved to a new session id, found through its transcript, and an agy conversation `/resume`d by a new agy in the same pane, whose next hook adopts it |
| Mail sent to an agent that left | The sender's next hook says so once, naming who now runs in that pane and who is in its tab |
| New session in a used pane | Gets a new handle; the previous session keeps its own |

Codex's ephemeral background sessions can inherit the interactive process's
Rex environment. Hooks that explicitly report `transcript_path: null` do not
take over the pane's identity or receive its mail. Interactive hooks with a
transcript keep normal startup, resume, and lazy-adoption behavior. Older
hooks that omit the field keep their existing behavior.

## State

`~/.local/state/rex-amq-adapter/` (override with `REX_AMQ_ADAPTER_HOME`):
`amq-root/` (the shared AMQ root), `bin/inject` (the wrapper wakers run), `records/` (one per handle),
`ledger/` (announced message ids), `window/` (join and departure notes waiting
for an agent's next prompt), `inject/<handle>.jsonl` (every doorbell
attempt and why it waited, trimmed past 1 MB; `who` shows the latest as LAST
DOORBELL, e.g. `deferred box_not_found 23m` when the adapter cannot find an
agent's input box, or `-` when no mail is waiting and the last attempt was not
accepted), `watch/` (locks of goal watchers), `logs/<handle>.log` (amq
wake), `logs/watch-<handle>.log` (a goal watcher), `logs/hooks.log`
(hook failures; hooks run with stderr discarded).

## Development

```bash
go test ./...                              # includes internal/e2e: scripted agents on a private Rex server and real amq wake
go test -short ./...                       # skips internal/e2e
REX_AMQ_LIVE=1 go test ./internal/rex/     # inside a Rex terminal: the rex CLI client against the live server
```

`internal/core` holds all policy and has no I/O; `internal/app` wires it to the
`rex` CLI (`internal/rex`), `amq` (`internal/amq`), and the state directory
(`internal/store`). `internal/screen` is the input-box classifier ported from
[herdr-amq-adapter](https://github.com/est7/herdr-amq-adapter).

## Adding another agent

[docs/adding-an-agent-kind.md](docs/adding-an-agent-kind.md) is the procedure:
what to probe in the new agent, which of its hooks map onto the adapter's
duties, the code to change, and the end-to-end checks to pass.

## Known gaps

- OpenCode is not supported.
- A departed handle's mailbox is kept; nothing archives old mailboxes yet.
- Cross-machine delivery is out of scope.
- An agent that ends a turn with mail it chose not to drain gets its next
  doorbell on amq's retry ladder, up to 2 minutes later.

## License

MIT. `internal/screen` is ported from herdr-projects (MIT); see `internal/screen/testdata/NOTICE.md`.
