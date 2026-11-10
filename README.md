# rex-amq-adapter

Coding agents running in [Rex](https://www.superlogical.com/rex/docs) terminals
message each other over [AMQ](https://github.com/avivsinai/agent-message-queue).
Install it once, and every Claude Code, Codex, Antigravity (`agy`), or Pi
session you start in a Rex terminal gets a mailbox under a handle of its kind
and a number (`codex-01`), keeps it when you resume the session in another
pane, and is told when mail arrives. Its role (`reviewer`, from the pane label
you launch it with, or taken later with `join`) is shown before the handle:
`reviewer(codex-01) | fix login`. No `amq init`, no environment variables,
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

Requirements: Rex (macOS), `amq` on `PATH` (tested with 0.127.2; needs
`amq wake check --json-schema 2` and `retire --if-generation`, as in 0.81.2+),
Go to build, and Claude Code (2.1.318), Codex (0.166.0), Antigravity CLI
(1.5.1), or Pi (1.4.0) — the versions tested.

```bash
brew install est7/tap/rex-amq-adapter      # macOS / Linux; pulls in amq
rex-amq-adapter setup                       # skill, hooks, and harness settings for every agent found; --dry-run to preview
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

Run one version for every hook. The adapter keeps no compatibility with state
an earlier version wrote: after an upgrade, stop the agents, remove the state
directory (`~/.local/state/rex-amq-adapter`), and start them again.

The hooks run the binary at the absolute path `setup` was run from.

`setup` appends five hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`,
`Stop`, `SessionEnd`) to `~/.claude/settings.json` and `~/.codex/hooks.json`,
leaving every other entry, key, and position as it was; adds a hook group named
`rex-amq-adapter` (`SessionStart`, `PreInvocation`, `Stop`) to Antigravity's
`~/.gemini/config/hooks.json` and a window `title` command to its
`~/.gemini/antigravity-cli/settings.json` (an existing title command of yours is
kept); installs a small JavaScript extension for Pi in
`~/.pi/agent/extensions/`; turns off Claude Code's agent view and Codex's shared
daemon (see Recommended configuration); and installs a skill stub whose guide
ships inside the binary. Run it again after moving the binary.

- **Codex** must be started with `codex --no-daemon` (resume:
  `codex --no-daemon resume <session-id>`). The shared background server runs
  every pane's threads with the first terminal's Rex environment, so the
  adapter refuses to adopt a thread it runs (`logs/hooks.log` says to restart
  with `--no-daemon`) rather than give one pane's identity and mail to
  another.
  Pass the flag explicitly when launching through Rex; fish abbreviations do
  not apply there. This mode cannot be combined with `--remote` or `codex agents`.
  Codex runs new hooks only after you approve them: run `/hooks` in Codex, or
  accept the review prompt at startup. The first hook that runs adopts the agent.
- **Claude Code** and **Antigravity** run hooks only in folders you trust. Trust is derived, never created: `rex-amq-adapter trust <dir>` (and `tab` for its `-c` directory) extends the trust you gave a repository in Antigravity to its linked worktrees, and a repository you never trusted keeps its dialog. Claude Code and Codex resolve worktrees to the main checkout themselves.
- **Antigravity** peers start with `agy --dangerously-skip-permissions -i "<brief>"`;
  without the skip flag agy stops to ask before every `amq` command it runs.
- An agent gets its mailbox when its first turn starts; Codex and Antigravity
  start none on their own, so give a peer its brief as the first prompt.

`rex-amq-adapter status` reports each item as `current`, `outdated`, `missing`, or
`foreign`; `rex-amq-adapter remove` deletes only what setup installed.

### Recommended configuration

The adapter finds an agent by the pane it runs in. Two harness features run
conversations in a shared background process that another terminal started,
so every conversation they host inherits that terminal's `REX_BLOCK`. `setup`
turns both off (the `supervisor` item) unless you set them on yourself, which
it keeps unless `--force`; `remove` leaves them off. A Codex daemon that is
already running is not stopped:

| Harness | Setting | Why |
|---|---|---|
| Claude Code | `"disableAgentView": true` in `~/.claude/settings.json` (or `CLAUDE_CODE_DISABLE_AGENT_VIEW=1`) | Agent view (`←` on an empty prompt, `/bg`, `claude --bg`) moves a conversation under Claude's supervisor. The adapter still places such a session by its pane's title, but one key press is enough to move a conversation there, and each one costs a supervisor, a pty host, and a second process beside the terminal that shows it |
| Codex | `daemon_auto_start = false` under `[features]` in `~/.codex/config.toml`; stop a running daemon with `codex app-server daemon stop` | The shared daemon runs every thread with the first terminal's environment, and the adapter adopts none of them. The setting keeps a plain `codex` from starting it; a daemon that is already running is still used unless Codex starts with `--no-daemon` |

Antigravity and Pi need nothing: each runs in its own pane's process.

## Use

Start agents in Rex terminals as usual. Then ask one to talk to another:

> ask codex to review the current diff over AMQ

Agents in one Rex session know each other: a session holds one task, a tab
in it one team, a pane one member. An agent learns who is in its session when
it starts, and the agents already there hear that it joined, or later left,
with their next prompt. "Ask the one beside you" needs no handle. Other
sessions are outside that awareness boundary: their agents do not appear in
discovery or join/leave notices.

A pane label names the agent's role, and a role with a prompt briefs the
agent when it starts. A tab named after a crew adds the crew's team
conventions, shared by every agent in it:

| Built in | What it is |
|---|---|
| roles `implementer`, `reviewer`, `qa`, `architect`, `writer`, `editor` | work in any tab |
| role `goal` | owns a goal and runs it unattended (below) |
| crew `pair` | implementer builds, reviewer critiques the working-tree diff (the task branch under autopilot), until clean |
| crew `ship` | `pair`, then QA runs the reviewed candidate and reports a verdict with evidence |
| crew `dev` | an architect decomposes the goal into tasks for an implementer and reviewer |
| crew `docs` | writers draft, an editor checks against the code; an optional architect partitions |
| crew `design` | architects send sealed proposals to the lead, debate a shared claim ledger, vote per claim, and report the agreed design and each split claim's crux |

```bash
rex-amq-adapter tab pair --role "implementer | fix #42" -- codex --no-daemon "Fix #42: …"
rex split -b <its block> --label "reviewer | review #42" -- claude "Review the implementer's fix for #42 when it hands off."
rex-amq-adapter crews        # every crew, its lead and members, and every role available here
```

The agent you are already talking to can lead the crew itself: asked to "open
a pair for #42", it runs `rex-amq-adapter join pair` in its own tab, which
names the tab `pair`, makes it the implementer (the crew's lead), relabels its
pane `implementer(claude-01)`, and prints its briefing with the members still
to start; it then splits the reviewer in beside it. Another agent already in
the tab keeps its crew: `join` refuses, and the crew goes in a new tab with
`tab`. `join --role R` takes a role without changing the tab.

A session without a `goal` agent assumes you are watching: an agent that stops
is yours to notice. Label a pane `goal` and the session runs unattended
instead. That
agent owns the goal and decides how to recover (its brief has the make-goal shape: GOAL, CONTEXT, SCOPE, BOUNDARIES with a `Reserved:` line, DONE WHEN; it loads skill `autopilot`, reviews and lands each task branch itself, and hands back per that skill): it is woken at once when a peer
quits, crashes, or has its pane closed, with the session to resume, and it
restarts or replaces the peer itself. Every other agent in the session is told that what its conventions, brief, or project rules would take to you goes to the goal agent instead; what still waits for you is the closed set in skill `autopilot` (Boundaries). If the goal agent is the one that goes, one
peer is told to report and stop; with no peer left, nobody is told. A small watcher process runs for each goal
agent and ends with it, and carries a watchdog for mail that cannot be delivered: unread mail whose doorbells have deferred with one reason for 30 s. On its own, once per pane and episode, it fixes one shape: a pane too small to draw the agent's input box (under 8 rows or 40 columns) whose doorbells find no box (`box_not_found`). Without touching what you look at, it detaches that block from its window (the agent keeps running; no client owns the terminal any more), resizes it to 120x30, which an unowned terminal takes at once, waits for the box to draw, hands the mail to the injector, and places the block in the session's `overflow` tab, where it keeps that size until you open the tab; before acting it takes the injector's lock for that pane and re-reads its agent, foreground, status, size, and box, and it never presses a key. Everything else it only reports to the goal agent, as mail in the stuck agent's name with the last twelve lines of its screen and what the watchdog had done to the pane: a trust dialog, a permission or other blocked prompt (that one also reaches your desktop as a notification), a draft in the box, a box missing at a normal size, any other deferral, a pane you moved out of the overflow tab (it is yours from then on), a resize the agent did not draw, or a placement that failed (the block then sits detached; `rex move` places it by hand, and the watchdog retries). Each step is logged to `logs/watchdog.log`; the episode itself lives in `watchdog/` so a restarted watcher continues from the block's actual state instead of detaching it again. When a peer's pane has fewer than 10 rows, start the next peer in a new tab rather than splitting it. In a crew tab the goal agent is the lead's stand-in for the human: its peer briefs open with make-goal's grant line, the implementer hands each task branch back to it, and the crew's reviewer (and QA, in `ship`) serves the goal agent, which asks them per task branch instead of the implementer's working-tree loop.

```bash
rex-amq-adapter tab pair --role goal -- claude "GOAL: ship #42. CONTEXT: issue #42, app/. SCOPE: push+PR. BOUNDARIES: Reserved: none. DONE WHEN: CI green on the task branch."
```

Your own crews and roles override the built-in ones by name: a crew is a
directory holding `team-conventions.md` and, optionally, `<role>.md` for a
role that works differently in it; `roles/<role>.md` holds roles for any tab.
They are read from `.agents/crews/` in the project (or a parent directory),
then `~/.config/rex-amq-adapter/crews/`. The format follows
[Runner](https://github.com/yicheng47/runner)'s crew examples, which the
built-in prompts are rewritten from.

The agent's skill handles `amq send`, `amq drain`, and `amq reply`, and opens
peers itself: a pane beside it for the same task, a new tab in its session for
another team on that task, a new session for another task or project. A peer
sends what it produces, a design doc or report included, back to the agent
that briefed it instead of opening a preview pane, so that agent decides what
you see. To start one by hand,
label its pane `"<role> | <task>"` (the agent takes the role and shows it
before its handle, the task after it) and give it a first prompt (an agent gets its mailbox
when its first turn starts):

```bash
rex split --split=right --focus=false --label "reviewer | review the diff" -- codex --no-daemon "Review the diff; report to claude over AMQ"
```

Inspect state:

```bash
rex-amq-adapter who          # running agents in this Rex session with their roles and tabs, including yourself
rex-amq-adapter who --all    # also this session's departed agents reserved for a resume
rex-amq-adapter events --json # stream this session's agents and mail as NDJSON (read-only; --tab, --session S, --all)
rex-amq-adapter await <block> # wait for an agent you started; prints its outcome as JSON
rex-amq-adapter collect      # retire agents whose pane closed or process died
```

An agent shows what it is working on after its role and handle in its pane's
label (`reviewer(claude-01) | fix login redirect`) with `rex-amq-adapter title
"<task>"`; its identity notice tells it to, and Claude Code, whose session
starts before the user asks anything, is asked once more at its first prompt.
A peer launched in a pane labelled `"<role> | <task>"` starts with that task.
The task follows the handle into any pane it is resumed in. Claude Code, Pi, and Antigravity show the same
label as their own title (Claude Code from the next prompt after a change, Pi
within about 30 s, agy at its next state change); Codex cannot without its
shared daemon.

`who` resolves the caller's current session from its identity (see
`env`); it requires a Rex terminal and fails if the session cannot be
resolved. It uses the current layout, so moving a block to another session
changes which session discovers it; its ROLE column shows each agent's role
and its TAB column the tab it is in now. `--all` includes departed agents last recorded in this session, never
agents from other sessions. A failure while reconciling the session is a
warning on stderr; the session is still listed. `who` also brings back a dead
waker in the session, so an idle agent hears its mail again once a peer checks
on it, and records where moved agents are now. Use the adapter's `who`, not the shared mailbox
directory from `amq who`.

## Lifecycle

| Moment | What happens |
|---|---|
| Agent's first hook runs | Handle chosen (`<kind>-<NN>`; the role, from the pane label, kept in the record), mailbox created, `amq wake` started, pane relabeled `<role>(<handle>)` (then `… \| <task>` once the agent names its task); it is told its session-mates and its crew and role briefing, and its session-mates are told it joined |
| Agent exits | `SessionEnd` retires its waker and tells its session-mates it left; the handle stays reserved for its session for 7 days |
| Agent crashes or its pane closes | The next adoption or `collect` notices and does the same, as does `who` in that session, a send to it, or within seconds a `goal` agent there |
| Session resumed (`claude --resume`, `codex resume`), in any pane | Gets its handle back, with the mail that arrived meanwhile; so does a Claude conversation that moved to a new session id, found through its transcript, and an agy conversation `/resume`d by a new agy in the same pane, whose next hook adopts it |
| Mail sent to an agent that left | The sender's next hook says so once, naming who now runs in that pane and who is in its session |
| New session in a used pane | Gets a new handle; the previous session keeps its own |
| Claude Code session moved to the background (`←`, `/bg`, `claude --bg`) | The pane that shows it, found by its title, is its pane; it is named afresh there, and the terminal session it came from departs. Until a pane shows it, it has no AMQ identity |

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
accepted), `watch/` (locks of goal watchers), `watchdog/` (open recovery episodes), `logs/watchdog.log`, `logs/<handle>.log` (amq
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

Run one test suite at a time: e2e hooks that exceed their budget under
several concurrent suites fail the test instead of retrying.

To release, run the tests, then push an annotated tag whose message body is
the release notes (`git tag -a vX.Y.Z -F notes.txt`, subject line `vX.Y.Z`).
`.github/workflows/release.yml` builds the four archives and `SHA256SUMS` and
publishes the release; then bump the formula in
[est7/homebrew-tap](https://github.com/est7/homebrew-tap).

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
- Antigravity's `/resume` of another conversation inside a running agy keeps
  the handle of the conversation it left.
- Antigravity subagents are recognized by agy's own conversation store
  (`brain/<parent>/.system_generated/subagents/`), which agy may change.

## License

MIT. `internal/screen` is ported from herdr-projects (MIT); see `internal/screen/testdata/NOTICE.md`.
