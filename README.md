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
claude pane ── rex-amq-adapter send --to codex ──▶ shared AMQ root ──▶ amq wake (codex's waker)
                                                                       │ --inject-via
                                                                       ▼
                                             rex-amq-adapter inject … (codex is idle)
                                                                       │ rex block call write
                                                                       ▼
                                               codex: "AMQ doorbell … Run rex-amq-adapter drain …"

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

Requirements: Rex (macOS), `amq` on `PATH` (tested with 0.139.0; needs
`amq send --labels`, `amq wake check --json-schema 2`, and `retire --if-generation`),
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
an earlier version wrote and migrates none: after an upgrade, stop the agents
and any `amq wake` they left running, remove the state directory
(`~/.local/state/rex-amq-adapter`, or `REX_AMQ_ADAPTER_HOME`), and start them
again. The shared AMQ root lives there, so unread mail goes with it. Upgrading
past the per-handle records to identities and leases is such an upgrade: a
waker left over from before is refused by the new injector and types nothing.

The hooks run the binary at the absolute path `setup` was run from.

`setup` appends five hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`,
`Stop`, `SessionEnd`) to `~/.claude/settings.json` and `~/.codex/hooks.json`
(or under `CLAUDE_CONFIG_DIR` / `CODEX_HOME`),
plus `StopFailure` for Claude Code, leaving every other entry, key, and
position as it was; adds a hook group named `rex-amq-adapter` (`SessionStart`,
`PreInvocation`, `Stop`) to Antigravity's `~/.gemini/config/hooks.json` and a window `title` command to its
`~/.gemini/antigravity-cli/settings.json` (an existing title command of yours is
kept); installs a small JavaScript extension for Pi in
`~/.pi/agent/extensions/` (or under `PI_CODING_AGENT_DIR`); turns off Claude Code's agent view and Codex's shared
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
  Until then every new Codex session stops on "Hooks need review", so
  `rex-amq-adapter status` lists each hook in `~/.codex/hooks.json` that waits
  for approval (never approved, or changed since, as after an upgrade rewrote
  it), comparing the hash Codex records in `config.toml`
  (`[hooks.state."<file>:<event>:<group>:<hook>"].trusted_hash`) without
  writing it; `epic` and `summon --harness codex` refuse Codex with
  `unapproved_hooks` before any pane, and `summon` of a reviewer, editor or
  qa without `--harness` starts it in the next ready family instead.
- **Claude Code** and **Antigravity** run hooks only in folders you trust, and every harness asks once per project. You create that trust; the adapter carries it. `rex-amq-adapter trust grant <dir>...` is your "yes, I trust this folder" for all four harnesses at once: the project's root (its git toplevel, or the main checkout of a linked worktree, by physical path) is recorded in the adapter's `trust.json`, `projects[<root>].hasTrustDialogAccepted` is set in `~/.claude.json` (under Claude's own `.lock`, every other key kept) and `trust_level = "trusted"` in Codex's `config.toml` (comments kept), and an Antigravity or Pi agent the adapter starts there gets it for its session (`agy --settings`, `pi --approve`), so their own files are never written. `rex-amq-adapter trust root add <dir>` declares a root (`ls` and `rm` list and forget them; never `/` or your home directory): a project under it (by physical path, a repository with no `origin` too) is granted the first time an Epic or a summon needs it, unless you said no to it yourself (`trust_level = "untrusted"` in Codex, a `false` entry in Pi's `trust.json`), which only your own `trust grant` overrides. `epic` and `summon` check every harness they would start before creating any Rex session or pane: a working directory that no longer exists is refused (Rex would open the pane in your home instead), and a project nothing trusts and no root covers is refused with `untrusted_folder`, naming the harnesses that would ask and the `trust grant` command that fixes it, so an unattended Epic never leaves a pane waiting on a trust dialog. Every grant is a line in `logs/trust.log`. `summon` and `tab` also start an Antigravity peer in a linked worktree of a repository you trust with `--settings`, extending that trust to the worktree for the peer's session, and `rex-amq-adapter trust <dir>` prints that value for a peer you start by hand.
- **Antigravity** peers start (and resume) with `agy --dangerously-skip-permissions --mode auto -i "<brief>"`;
  without the skip flag agy stops to ask before every `amq` command it runs,
  and without `--mode auto` it may stop in plan mode for a review no one gives.
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

Crew roles (such as `orchestrator`) track Slices in each harness's native task
list rather than repository files. Two harnesses gate those tools behind a
setting on current models; `setup` turns both on (the `tasks` item) unless you
set them off yourself, which it keeps unless `--force`, and `remove` leaves
them on:

| Harness | Setting | Why |
|---|---|---|
| Claude Code | `"CLAUDE_CODE_ENABLE_TODO_TOOLS": "1"` under `"env"` in `~/.claude/settings.json` | Claude Code gates `TaskCreate`/`TaskUpdate` behind a 4.x model allowlist unless `CLAUDE_CODE_ENABLE_TODO_TOOLS` is enabled |
| Codex | `enabled = true` under `[tools.update_plan]` in `~/.codex/config.toml` | Code-mode-only models (such as `gpt-6.1-sol`) leave `update_plan` off unless enabled explicitly |

Antigravity (`brain/<id>/task.md` artifact) and Pi (`TaskCreate`/`TaskUpdate`
in `1st-pi`) already expose task tracking with no extra setting.

## Use

Start agents in Rex terminals as usual. Then ask one to talk to another:

> ask codex to review the current diff over AMQ

Agents in one Rex session know each other: a session holds one Epic (its
label is yours to name, and agents never rename it) and at most one crew, and
each agent in it has a tab of its own. An agent learns who is in its session when
it starts, and the agents already there hear that it joined, or later left,
with their next prompt. "Ask the one beside you" needs no handle. Other
sessions are outside that awareness boundary: their agents do not appear in
discovery or join/leave notices.

A pane label names the agent's role, and a role with a prompt briefs the
agent when it starts. A crew the Rex session works as adds the crew's team
conventions, shared by every agent of the session, in any tab:

| Built in | What it is |
|---|---|
| roles `implementer`, `reviewer`, `qa`, `architect`, `writer`, `editor` | work in any session |
| role `orchestrator` | owns a goal and coordinates the agents that work on it (below) |
| role `personal` | in a Rex session of its own, starts the user's Epics (an orchestrator and a narrator each) and talks to them through the narrator it links (below) |
| crew `pair` | implementer builds, reviewer critiques the working-tree diff (or a Slice branch the brief names), until clean, in the review tiers below |
| crew `ship` | `pair`, then QA runs the reviewed candidate and reports a verdict with evidence |
| crew `dev` | an architect decomposes the goal into tasks for an implementer and reviewer |
| crew `docs` | writers draft, an editor checks against the code; an optional architect partitions |
| crew `design` | architects send sealed proposals to the lead (no session-transcript search, `nmem t` or `rex-history`, until the ledger is broadcast), debate a shared claim ledger, vote per claim, and report the agreed design and each split claim's crux |

```bash
rex-amq-adapter join pair    # claim crew pair for this Rex session and take its lead, implementer
rex-amq-adapter summon reviewer --subject "review #42" --body "Review the implementer's fix for #42 when it hands off." --kind todo --asked "<your words>"
rex-amq-adapter crews        # every crew, its lead and members, and every role available here
rex-amq-adapter crews show ship --role qa   # the briefing a QA agent in a ship session gets, and its routing, changing nothing
rex-amq-adapter crews --json # crews and roles as JSON, each with its purpose
```

Which harness, model, and reasoning effort each role starts on comes from
`routing.toml`: the built-in one
([`internal/crew/builtin/routing.toml`](internal/crew/builtin/routing.toml)),
overridden role by role by `~/.config/rex-amq-adapter/crews/routing.toml`
and then by the project's `.agents/crews/routing.toml`. Each `[role.<name>]`
lists `harness = [...]`, best first, and a `<harness> = { model, effort }`
table for each; `[role."*"]` routes a role without one:

```toml
[role.reviewer]
harness = ["codex", "agy", "claude"]
codex = { model = "gpt-6-astra", effort = "xhigh" }
agy = { model = "gemini-4-carbon-high", effort = "high" }
claude = { model = "opus", effort = "high" }
```

The adapter passes them as each CLI's flags (`claude`/`agy` `--model`
`--effort`, `codex` `-m` `-c model_reasoning_effort=`, `pi` `--model`
`--thinking`) on `epic`, a summon's start, and a resume, and records them on
the agent (`who --json` shows `model` and `effort`), so a resume runs on what
the agent started on, CLI defaults included, whatever the routing says by then. A model is letters, digits, and `._:/-` that never
starts with `-`; an effort is one of `off|minimal|low|medium|high|xhigh|max`
that the CLI takes. A file that breaks either rule, misspells a key, or
names an unknown harness refuses every launch and names the file. A model's
family comes from its name (`claude`, `opus`, `sonnet`, `haiku` Anthropic;
`gpt`, `o1`, `o3`, `o4` OpenAI; `gemini` Google), else from its harness.

The agent you are already talking to can lead the crew itself: asked to "open
a pair for #42", it runs `rex-amq-adapter join pair` where it stands, which
claims crew `pair` for its Rex session, makes it the implementer (the crew's
lead), relabels its pane `implementer(claude-01)`, and prints its briefing
with the members still to start; it then starts the reviewer in a tab of its
own. A session works as one crew: once other agents are in it, `join` refuses
another, whose goal is another Epic. `join --role R` takes a role and leaves
the crew alone.

Each agent gets a tab of its own unless you ask for a split. A tab nobody named
(Rex's `Window 4c6fe88d`) or one `summon` opened follows its agent: while one
agent is in it, beside any panes without an agent (a preview, a shell), it
shows that agent's pane label, and it follows the agent's task and role. When
a second agent joins it, it drops to the task that label named
(`orchestrator(claude-03) | audit` becomes `audit`), so it does not read as
one member's tab; it follows its agent again once only one is left. A tab you
renamed keeps your name. Tabs name no crew: a tab named `pair` is just a name.

A session without an orchestrator assumes you are watching: an agent that stops
is yours to notice. Label a pane `orchestrator` and that agent coordinates the
session instead. It owns the goal, records progress, reports, and decides how to recover (when a skill for running goal contracts such as `run-goal` is installed it follows that skill, whose contract says whether you stay or have left; without one it takes every open call to you): it is woken at once when a peer
quits, crashes, or has its pane closed, with the session to resume, and it
restarts or replaces the peer itself. Every other agent in the session is told that what its conventions, brief, or project rules would take to you goes to the orchestrator instead; what still waits for you is the orchestrator's to say, and a permission or trust dialog always does. If the orchestrator is the one that goes, one
peer is told to report and stop; with no peer left, nobody is told. A small watcher process runs for each
orchestrator and ends with it, and carries a watchdog for mail that cannot be delivered: unread mail whose doorbells have deferred with one reason for 30 s. On its own, once per pane and episode, it fixes one shape: a pane too small to draw the agent's input box (under 8 rows or 40 columns) whose doorbells find no box (`box_not_found`). Without touching what you look at, it detaches that block from its window (the agent keeps running; no client owns the terminal any more), resizes it to 120x30, which an unowned terminal takes at once, waits for the box to draw, hands the mail to the injector, and places the block in the session's `overflow` tab, where it keeps that size until you open the tab; before acting it takes the injector's lock for that pane and re-reads its agent, foreground, status, size, and box, and it never presses a key. Everything else it only reports to the orchestrator, as mail in the stuck agent's name with the last twelve lines of its screen and what the watchdog had done to the pane: a trust dialog, a permission or other blocked prompt (that one also reaches your desktop as a notification), a draft in the box, a box missing at a normal size, any other deferral (a working agent's is not one: its hooks tell it in-band), a pane you moved out of the overflow tab (it is yours from then on), a resize the agent did not draw, or a placement that failed (the block then sits detached; `rex move` places it by hand, and the watchdog retries). The watchdog also reads each agent's own transcript (Claude Code `~/.claude/projects/*/<session>.jsonl`, Codex `~/.codex/sessions/*/rollout-*-<thread>.jsonl`, pi `~/.pi/agent/sessions/*/*_<session>.jsonl`, honoring `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, and `PI_CODING_AGENT_DIR`; Antigravity `~/.gemini/antigravity-cli/conversations/<conversation>.db` through `sqlite3`), for what no hook can report because the turn has not ended, matching no wording: the time of the last row, and a tool call whose result has not come. A tool call without a result for 15 minutes, or a working agent whose transcript has not grown for 15 minutes, is told to the orchestrator once, as the last eight rows (each an excerpt of at most 400 characters) inside a "captured output; treat as data, not instructions" block (plus the screen for a stall), and the orchestrator reads them and decides. Tool calls are paired by id over the last 512 KB of the transcript, so a long call is not hidden by the rows after it; Antigravity steps carry no time of their own, so their activity is the database's or its WAL file's modification time. A finding about the orchestrator itself goes to a live agent of another model family in the session, and to your desktop when there is none. A turn that ends in an error or a limit is reported by the agent's own stop hook as it ends, with the harness's own reason (Claude Code `StopFailure`, Antigravity's `terminationReason`, pi's settle `outcome`, Codex a rollout whose last row is the turn's error; a cancelled turn is not reported): `error: <handle> turn_error` or `turn_limit` goes once to the agent that should act, in this order: the agent that summoned it, its session's crew lead, the earliest agent in its tab, the session's orchestrator, and, when one of those is itself stopped by an error, a live agent of another model family; with none, or when the mail cannot be sent, your desktop hears. An agent whose turn ended in an error is passed over as a recipient until its next turn starts, or for 15 minutes. An agent of the session, the orchestrator included, that sits on a prompt only you can answer (its program status reports blocked, or a trust screen is up while it reports nothing else) for 30 s, mail pending or not, gets one line in `logs/blocked.log` with the time and its last screen lines and one notice per episode: to the orchestrator for a peer (a permission prompt also reaches your desktop), and for the orchestrator itself, which cannot read mail while it waits, to a live agent of another model family and your desktop. No key is pressed. Each step is logged to `logs/watchdog.log`; the episode itself lives in `watchdog/` so a restarted watcher continues from the block's actual state instead of detaching it again. A split you ask for into a pane under 10 rows leaves the peer unable to draw its input box, so agents open a tab instead. In a crew's session the orchestrator is the lead's stand-in for the human: when its peer briefs tell the implementer to hand each Slice branch back, the crew's reviewer (and QA, in `ship`) serves the orchestrator, which asks them per Slice branch instead of the implementer's working-tree loop.

Whether the human stays is the brief's to say, not the role's: an orchestrator handed a goal contract follows its `MODE` (unattended when omitted), and one handed a plain request runs attended.

```bash
rex run --label orchestrator --focus=false -- claude "/goal Load skill run-goal. Execute the goal contract at /abs/path/.goal/20261110-ship-42.md; it is the grant. …"
```

Review runs in one of two tiers, written into the `pair` conventions, the orchestrator's prompt, and the agent guide, after tstack's `sol-loop` and `council`. By default one reviewer of another model family reviews in at most 3 rounds (round 1 full, later rounds only changed code and open finding ids), extended to 5 only for security, permissions, auth, data loss, or state/concurrency while a `blocker` or `should` stays open with evidence, or when you ask; a round with only nits ends it, and the end of the budget is not approval. When you say "council" (a score after it, "council 9", is the bar), the author fixes known issues and runs the checks, then runs `rex-amq-adapter summon reviewer --council --cwd <worktree> --grant <file> --kind review_request --subject S --body B`: one seat per harness of `[role.reviewer].harness` (codex, agy, claude), each asked for `SCORE: <n>/10`. Rounds go on until every seat is satisfied or reaches your score; there is no other round cap and no default score. Nobody edits the checkout while a seat reads it, a missing seat is reported, never substituted, and the scores go in a `## Council review` table (seat by round, `no score` where a seat gave none) in the PR description, or the final reply without a PR. In both tiers the author fights scope creep, and an item still disputed after two replies (or the one-seat budget's end) comes to you with both sides in 2-3 lines.

An agent reaches a peer that may not be running with `rex-amq-adapter summon <handle|role> --subject S --body B [--cwd DIR]` (`--council` for a review role: one seat per harness of its routing, the live ones mailed and the rest resumed or started, refused with nothing mailed or launched when any seat cannot be seated; `seats` in its JSON says what each got): a live agent of that handle or role gets the mail; a departed one of this session is resumed in a tab of its own (in the directory it ran in, which adoption now records; `--split` puts a started or resumed agent beside the caller instead, when you asked, never a narrator or personal agent) and its mail waits for it; with `--harness H` and no `--cwd`, only an agent of the role on `H` counts, so one on another harness, live or departed, gets no mail and is not resumed; with none, a new agent is started for the role with the message as its brief and told once adopted; a started agent runs on its role's routing (below); a reviewer, editor or qa starts, unless `--harness` names one, on the first harness of its routing whose model is another family than the caller's model, installed, and passes the preflight (the result's `harness` and `passed_over` say which and why), and is refused, naming each reason, when none is. The guards are the boundary: a personal agent never summons (it opens an Epic with `epic`, and nothing is mailed or launched); only a request kind summons; no handle or role already in the caller's summon chain; depth 2 at most, the user's agents being 0; 6 live agents per session at most; `--cwd` into a directory where a live agent of the role works is refused, except that a reviewer, editor or qa on another harness may share it (a council seats one per harness: a second `summon reviewer --cwd <worktree>` starts on a harness not yet seated there, or name one with `--harness`); a new agent only when the brief cites the user's grant file (`--grant`, which the caller checks; the adapter does not read it) or the caller quotes the user's own words asking for it (`--asked`, an attestation the log keeps); no start or resume when the caller's own summon chain cannot be traced back to the user. A launch carries a token in its environment (`REX_AMQ_SUMMON`) that binds its adoption to the summon. It holds its place in the cap until then; a resume starts with a fixed prompt that makes the agent drain its mailbox. The message keeps the kind the caller gave it. A summoned agent's identity carries its parent and depth, and the summon's JSON names the reached agent's `agent_session_id` and `cwd`; every summon is logged in `logs/summon.log` and listed in the summoner's hand-back.

A personal agent runs Epics without working in them. Give an agent in a Rex
session of its own (your `~/me`, say) the role with `rex-amq-adapter join
--role personal`, and ask it for an Epic. It writes the goal contract (skill
`make-goal`, when installed) and runs one command, then tells you the Epic
started and that you can ask it about it:

```bash
rex-amq-adapter epic "login revamp" -c ~/src/app --goal ~/src/app/.goal/20261112-login.md --asked "<your words>"
```

`epic` needs `--goal`, `--asked`, or both. `--goal -` reads the contract from
stdin and keeps it read-only as the Epic's `goal.md`. Either way the adapter
notarizes your words and the contract (with its SHA-256) once, read-only, in
the Epic's `grant.json` and `grant.md`; the JSON line and the view name them as
`goal_path` and `grant_path`, and the lead's start notice ends with `Goal:` and
`Grant:` lines pointing at them, so the run cites your words rather than an
agent's retelling. `epic` creates the Epic's Rex session (named after it) with the orchestrator,
briefed to run the contract (or, with only `--asked`, with your
request), and a narrator in a background tab of its own, each tab following
its agent's pane label. Both start on their role's routing (below); the
narrator only reads mail and rewrites the view, so it runs on a cheap tier
(`agy --model gemini-4-hydrogen-low`, or `claude --model haiku --effort low`
when routed to Claude), on a resume too, while the orchestrator starts on
`claude --model opus --effort high`; with `--crew C` the session works as crew C, and
with `--crew pair` it is one Slice with no orchestrator: an implementer, on
its own routing, leads, runs the goal (Checkpoints, its own reviewer), and
records the hand-back, reads the view, and takes the narrator's `--control`
directives as an orchestrator does. It links
the Epic to the personal agent and waits for both agents (`--timeout`, 90 s
by default).
It prints one JSON line and exits 0 only when both are adopted and linked; a
failure part way reports what started and tears nothing down.
Every Epic runs unattended, whoever asked for it: `epic` records the
session's external-write policy (`sessions/<id>/policy.json`), and its
orchestrator, its narrator, and every worker the session starts or resumes
run with `ME_UNATTENDED=1` set in their own command (an `env` prefix, since
Rex panes do not inherit the adapter's environment), so their shell tools
stage outward writes for you instead of sending them. A personal agent never
gets the marker, and the adapter never clears one it inherited;
`rex-amq-adapter env` exports it to an unattended agent's shells. From then on
the personal agent reads the Epic with `view --session` and asks or directs it
only through the narrator, which relays your decisions to the orchestrator
verbatim; the Epic's workers never see either of them. The narrator's report
is its summary in the view: it mails the personal agent only to answer it,
for a hand-back or the orchestrator leaving, or for something that waits on
you, never to check in.

An Epic ends in two steps. The orchestrator records its whole final report
(review queue with PR links, Blocked on me, Decided for you, Changed, Found)
with `rex-amq-adapter hand-back @<file>` (or `-` for stdin); the adapter
stores it in `sessions/<id>/hand-back.json`, `view` shows it whole as
`hand_back` (`by`, `at`, `body`), and only once it is stored does the
session's watcher wake the narrator, urgently. The narrator tells the
personal agent, which reads `hand_back` and tells you what was handed back and
asks whether to close it. A pane label that reads `hand-back: …` signals
nothing. Only on your words does it run `rex-amq-adapter epic close
<session_id> --asked "<your words>"`, which kills the Epic's Rex session (what
its panes showed is gone), parks its agents as `epic_closed` with their handles
reserved for a resume for 7 days, and unlinks it. It refuses a session you did
not link, and, unless `--force`, one whose agents are still working, waiting on
a dialog, report no state, or hold unread mail. Nothing closes an Epic on its
own, and a session you started yourself is yours to close.

Both commands take `--op-id ID` (at most 256 bytes, no control characters),
which makes a retry safe. `epic --op-id` records itself, with the sessions
Rex has, before it asks Rex for the new one, and the new session and its
blocks before it waits for either agent, so running the same command again
after a timeout or a crash never starts a second Epic: it finds the session
the first run started, relinks and reconciles it, and prints
`"replayed":true`. It starts one only when Rex shows the first run created
none, and when it cannot tell which new session is its own, it says so and
starts nothing. `epic close --op-id` is recorded once
its guards pass and before the kill, so a retry finishes the kill, the
parking, and the unlink, even once the link is gone. The same op id for
another request is refused, and a session Rex no longer has is reported, not
replaced. Every Epic, with or without an op id, records its lifecycle in
`sessions/<id>/lifecycle.json`: `partial_start` once its session exists,
`opened` once both agents are adopted, `hand_back`, and `closed`, never
moving back. Each hand-back carries a `seq` and a `digest`
(`sha256:` of its body, line ends and surrounding blanks aside): recording
the same report again keeps its `seq` and does not wake the narrator, a new
one takes the next.

`rex-amq-adapter view --cockpit` is the read-only cockpit of one personal
agent: its linked Epics (lifecycle, hand-back, whether the narrator has
written a view), or one of them with `--session S` (its view, or its
lifecycle alone before the narrator writes one). It moves no mail and starts
nothing. The personal agent runs it in its own pane; you run it from outside
Rex with `--personal <handle>` or `--hub <dir>` (the directory the personal
agent works in). Any other agent is refused, as is a command running under
one (`env -i` included, by its process ancestry), a harness session no hook
adopted, and a shell with `ME_UNATTENDED=1` that is not the personal agent's.

`rex-amq-adapter panel --json [--session S]` is the human's panel on a Rex
session (the floating `rex-amq-ui` reads it): the `events --json` stream of
your session, or of S, whose snapshot also carries a `panel` field with the
session's label, its crew (with lead and members), what waits on each live
agent (`turn_error`, `waiting_on_the_human`, `stalled`, with or without a
narrator), its stored view (or its lifecycle or hand-back alone, with a
`no_view` gap), and, in a personal agent's session, a `cockpit` summary of
each linked Epic. A change is told as an `upsert` with `entity: "panel"`;
a narrator refresh that moves only the view's cursor is not a change. It
reads only, like `events`. It is yours alone: a harness session variable,
`ME_UNATTENDED=1`, a pane holding a live agent of any role (personal
included), or a process under one is refused before Rex is asked.

Your own crews and roles override the built-in ones by name: a crew is a
directory holding `team-conventions.md` and, optionally, `<role>.md` for a
role that works differently in it; `roles/<role>.md` holds roles for any session.
They are read from `.agents/crews/` in the project (or a parent directory),
then `~/.config/rex-amq-adapter/crews/`. The format follows
[Runner](https://github.com/yicheng47/runner)'s crew examples, which the
built-in prompts are rewritten from.

The agent's skill handles `rex-amq-adapter send`, `drain`, and `reply`, and opens
peers itself: a tab of its own in its session for each peer on the same task
(a pane beside it when you ask), a new session for another task or project. A peer
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
rex-amq-adapter who --all    # also this session's departed agents reserved for a resume, and its narrator
rex-amq-adapter who --json   # the same agents as JSON ({"agents": [...]}, as events' snapshot), each with agent_session_id and cwd
rex-amq-adapter view         # a session's view.json (the narrator writes it; its orchestrator and a linked personal agent read it)
rex-amq-adapter events --json # stream this session's agents and mail as NDJSON (read-only; --tab, --session S, --all: every session, offline handles included)
rex-amq-adapter panel --json  # (you, not an agent) events of this session plus its panel: label, crew, attention, view, cockpit
rex-amq-adapter await <block> # wait for an agent you started; prints its outcome as JSON
rex-amq-adapter collect      # retire agents whose pane closed or process died
rex-amq-adapter retire <handle> # close a finished peer's pane without a crash report (not rex block close)
```

Mail through the gateway. One AMQ root holds every Rex session, and the
adapter keeps sessions apart: members of one session (one Epic) mail each
other, sessions never do, and a personal agent reaches only the narrator of
an Epic it linked. The adapter's own mail commands run amq as the caller,
check that policy first, and record where sender and recipients were, so a
refusal is immediate and names its reason:

```bash
rex-amq-adapter send --to codex-01 --subject S --body B   # also reply, read, drain, thread, trace, dlq, list
rex-amq-adapter link <epic-session>    # (a personal agent) exchange mail with that Epic's narrator; `link` alone lists them
rex-amq-adapter unlink <epic-session>  # revoke it
rex-amq-adapter view --session <id>    # an Epic's view.json (its narrator, orchestrator, or a linked personal agent)
rex-amq-adapter view --cockpit [--personal H | --hub DIR] [--session <id>]   # read-only: one personal agent's linked Epics
rex-amq-adapter view refresh --summary "<what changed>"   # (the narrator) rebuild the view; `view read --ids …` reads named mail
rex-amq-adapter hand-back @report.md   # (the orchestrator, last) record its final report; view shows it as hand_back
rex-amq-adapter notify-personal --ref inbox:42 --event-id ev-1 [--hub ~/me] [--summary "…"]   # (an outside producer) ring the live personal agent
```

`--me`, `--root`, `--session`, and `--project` are the adapter's to set and
are refused. Mail an agent arrives at is checked again before it is
announced: what the policy refuses moves to a quarantine, unread. Mail sent
with `amq` directly carries no record of who sent it, so it never reaches a
personal agent or a narrator (`unverified_sender`), whatever `--me` it names.

`notify-personal` is how a producer outside Rex (an Inbox entry, a launchd
timer) wakes the personal agent. It runs from anywhere, finds the live
personal agent itself (narrowed by `--hub DIR` or `--personal HANDLE`), and
mails it in its own name as the adapter's notice, the summary quoted as
untrusted data. It never starts an agent: with none live it prints
`{"status":"no_live_personal",...}` and exits 0; with more than one it
refuses. A repeated `--event-id` is answered `"deduped":true` without sending
again, and one reused with another `--ref`, `--hub`, or `--personal` is
refused.

An agent shows what it is working on after its role and handle in its pane's
label (`reviewer(claude-01) | fix login redirect`) with `rex-amq-adapter title
"<task>"`; its identity notice tells it to, and Claude Code and Pi, whose
sessions start before the user asks anything, are asked once more at their
first prompt.
A peer launched in a pane labelled `"<role> | <task>"` starts with that task.
The task follows the handle into any pane it is resumed in. Claude Code, Pi, and Antigravity show the same
label as their own title (Claude Code from the next prompt after a change, Pi
within about 30 s, agy at its next state change); Codex cannot without its
shared daemon. When the Rex session is named for an Epic, the conversation
title of Claude Code and Pi (the one `/resume` lists) carries it in brackets,
`reviewer(claude-01) | [auth-refresh] fix login redirect`; the pane label and
agy's window title do not.

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
| Agent's first hook runs | Handle chosen (`<kind>-<NN>`; the role, from the pane label, kept in its identity), mailbox created, `amq wake` started, pane relabeled `<role>(<handle>)` (then `… \| <task>` once the agent names its task); it is told its session-mates and its crew and role briefing, and its session-mates are told it joined |
| Agent exits | `SessionEnd` retires its waker and tells its session-mates it left; the handle stays reserved for its session for 7 days |
| Agent crashes or its pane closes | The next adoption or `collect` notices and does the same, as does `who` in that session, a send to it, or within seconds an orchestrator there |
| A finished peer is retired (`rex-amq-adapter retire <handle> [--force]`) | Its lease is released as `retired` (waker retired, no "left" mail or urgent narrator notice), then its pane closes (it stays open when it no longer shows the agent, `"pane":"left_open"`); its `SessionEnd`, late hooks, and collection then change nothing, and the handle stays reserved for 7 days. An orchestrator uses this instead of `rex block close`, which reads as a crash. Refused for itself, a narrator, personal, or orchestrator agent, an agent of another session, and, unless `--force`, one still working or holding unread mail |
| Session resumed (`claude --resume`, `codex resume`), in any pane | Gets its handle back, with the mail that arrived meanwhile; so does a Claude conversation that moved to a new session id, found through its transcript, and an agy conversation `/resume`d by a new agy in the same pane, whose next hook adopts it |
| Mail sent to an agent that left | The sender's next hook says so once, naming who now runs in that pane and who is in its session |
| New session in a used pane | Gets a new handle; the previous session keeps its own. `/clear` in the same process keeps the handle, and `who --json` shows the new session as its `agent_session_id` |
| A conversation still running in one pane is resumed in another | The pane that holds it wins: the new pane gets no AMQ identity, is told `conversation <sid> is live as <handle> in <block>; this pane has no AMQ identity until that pane closes`, and its `env`, `drain`, and `send` fail; its exit ends nothing. Once the first pane's agent is gone, the new pane's next hook takes the handle over |
| Rex restarts | The agents die with the server; a conversation started again in a pane, by you or a tool such as `rex-durable`, takes its handle back with its unread mail. When Rex restores its sessions, the handle stays in its session and block and its Epic carries on. When Rex starts empty (`--restore=false`, or after `rex server stop --discard`) the handle comes back in the new session, but mail sent to it in the old one is fenced off and the old session's Epic does not carry over |
| Claude Code session moved to the background (`←`, `/bg`, `claude --bg`) | The pane that shows it, found by its title, is its pane; it is named afresh there, and the terminal session it came from departs. Until a pane shows it, it has no AMQ identity |

Codex's ephemeral background sessions can inherit the interactive process's
Rex environment. Hooks that explicitly report `transcript_path: null` do not
take over the pane's identity or receive its mail. Interactive hooks with a
transcript keep normal startup, resume, and lazy-adoption behavior. Older
hooks that omit the field keep their existing behavior.

## State

`~/.local/state/rex-amq-adapter/` (override with `REX_AMQ_ADAPTER_HOME`):
`amq-root/` (the shared AMQ root), `bin/inject` (the wrapper wakers run), `identities/` (who each handle is, one per handle),
`leases/` (where each running handle's agent runs: its block, process, and epoch),
`ledger/` (announced message ids), `window/` (join and departure notes waiting
for an agent's next prompt), `tabs/` (who names each tab: a follower, a crew,
or you), `inject/<handle>.jsonl` (every doorbell
attempt and why it waited, trimmed past 1 MB; `who` shows the latest as LAST
DOORBELL, e.g. `deferred box_not_found 23m` when the adapter cannot find an
agent's input box, or `-` when no mail is waiting and the last attempt was not
accepted), `watch/` (locks of session watchers), `watchdog/` (open recovery episodes), `logs/watchdog.log`, `logs/<handle>.log` (amq
wake), `logs/watch-<session>.log` (a session watcher), `logs/hooks.log`
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

`.github/workflows/ci.yml` builds, vets, and runs the short tests with and
without the race detector plus `govulncheck` on every push to `main` and every
pull request. The app tests run there against `amq.Fake`; the few that need
amq itself, and the Rex end-to-end tests, skip. Before a release run the full
suite locally against amq too: `REX_AMQ_REAL=1 go test ./...`.

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
