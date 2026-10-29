# rex-amq-adapter design

Coding agents running in [Rex](https://www.superlogical.com/rex/docs) terminals
message each other over [AMQ](https://github.com/avivsinai/agent-message-queue).
Install once; every Claude Code, Codex, Antigravity (`agy`), or Pi session started in a Rex terminal gets
a mailbox under a stable handle and is told when mail arrives. No `amq init`, no
environment variables, no manual naming.

This document is the contract for the first implementation. Facts about Rex and
the agents are marked with the evidence behind them:

- **executed**: observed in this repository's probes against Rex
  `0.0.1792158932-g88fd41`, Claude Code 2.1.303, Codex 0.163.0 (2026-10-18).
- **located**: read from documentation or a binary, not exercised.
- **asserted**: design intent, not yet verified.

## 1. Problem

An AMQ message is only useful once the recipient agent reads it. Delivery has
three requirements:

1. **Timely**: the agent learns about mail at the next moment it can act on it.
2. **Non-destructive**: delivery never corrupts what the user is typing, never
   answers a dialog, never interrupts a tool call.
3. **Observable**: the adapter can tell whether the notice reached the agent.

The Herdr adapter (`est7/herdr-amq-adapter`) meets these by typing a doorbell
into the agent's input box, guarded by screen heuristics. That has one channel
and two weaknesses: heuristics guess at the agent's state, and success proves
only that keystrokes were written.

Rex plus the agents' own hook systems offer stronger primitives. This design
uses them so that typing is the last resort, not the only channel.

## 2. Primitives

| Primitive | What it gives | Evidence |
|---|---|---|
| OSC 7501 program status, read with `rex block call -b B program_status` | The agent's own report of `idle` / `working` / `done` / `blocked` (+ `kind`, `msg`, `owner.pid`) | executed: Claude reports `app=claude-code`, Codex `app=codex`; Claude ends a turn in `done`, Codex in `idle`; Claude reports `blocked kind=question` with the question text while AskUserQuestion is open |
| `rex block call -b B process` | Foreground pid, name, argv, cwd of the block | executed |
| `rex capture --format=vt` | Screen with SGR styling | executed: herdr's `screen.Check` classifies it unchanged (`empty` for placeholder text, `typed` for a draft) |
| `rex block call -b B write {"data":…}` / `rex send` | Raw bytes to the pty; no bracketed paste is added | executed |
| `REX_SESSION`, `REX_BLOCK`, `REX_SERVER` | Set in every Rex terminal; hooks and tools must inherit the pane's environment | executed; Codex shared-server exception below |
| Codex `--no-daemon` | Keeps hooks and tools under the terminal-launched Codex process | executed with Codex 0.164.0 (2026-10-25): the shared server was parented by launchd and tool shells lacked `REX_BLOCK` / `REX_SESSION`; a separate Rex probe with `--no-daemon` inherited both, tools were descendants of its Codex process, and adapter `env` returned the assigned handle. This probe verified identity, not a full AMQ round trip |
| Block label | `shell` by default (also for `rex run -- cmd`); `--label X` sets it | executed |
| CLI exit status 3 | Target not found or ambiguous | located (`docs/automate/cli`) |
| Claude/Codex `Stop` hook `{"decision":"block","reason":…}` | Agent continues its turn with `reason` as input | executed on both |
| Claude/Codex `PostToolUse` `hookSpecificOutput.additionalContext` | Text added to the model context mid-turn | executed on both |
| Hook input `stop_hook_active`, `session_id`, `turn_id` | Loop guard and turn identity | executed on both |
| `SessionStart` hook stdout | Plain text becomes agent context | executed (Rex's own hook) |
| `SessionEnd`, `UserPromptSubmit` hooks | Lifecycle and user-turn entry points | executed (adapter e2e on both agents) |
| Codex hook approval | New or changed hooks run only after the user approves them in a review screen; trust is keyed by source, event, position, and content hash | executed |
| Codex `SessionEnd` timeout | Clamped to 3 s | executed (startup warning) |
| Codex approval dialog | Program status `blocked` with no `kind`; the screen classifier reads the box as `typed`; both defer a doorbell | reproduced (independent review, hook-less Codex in a scratch tab) |
| Antigravity (`agy` 1.4.2) program status | `app=antigravity`; `working` / `done` / `idle`, and `blocked kind=permission` with the command while it asks to run one | executed |
| Antigravity hooks | `~/.gemini/config/hooks.json` (or a project's `.agents/hooks.json`) maps a hook name to events `SessionStart`, `PreInvocation` (before each model call), `PostToolUse`, `Stop`; camelCase JSON in and out; `injectSteps` adds context, Stop `{"decision":"continue","reason":…}` keeps the turn going; no `SessionEnd`, no `stop_hook_active`; Stop also runs on cancel and errors (`terminationReason`) | located (agy's bundled `agy-customizations/docs/hooks.md`); SessionStart, PreInvocation, and Stop executed |
| Pi (0.84.2) extensions | A JavaScript or TypeScript file in `~/.pi/agent/extensions/` (or `pi -e <file>`) runs in Pi's process: `pi.on("session_start" / "agent_start" / "agent_before_settle" / "agent_settled" / "session_shutdown")`, `pi.sendMessage(msg, { triggerTurn })` starts a turn when idle, `{ deliverAs: "steer" }` lands in a running turn, `agent_before_settle` may return `{ entries, continue: true }`; `session_shutdown.reason` is `quit`, `reload`, `new`, `resume`, or `fork`; Pi reports OSC 7501 (`app=pi`) | located (bundled docs and `types.d.ts`); every path the adapter uses executed |
| Antigravity input box | `>` under a rule, continued to the next rule (Claude's layout with another glyph); `ctrl+u` clears it; bracketed paste + CR submits | executed |

Rex has no plugin runtime: no manifest, no lifecycle hooks, and `init.lua`
receives no events (located: `docs/automate/lua-events`). Every entry point
below is therefore either an agent hook or the injector that `amq wake` runs.
There is no adapter daemon.

### Typing findings (executed)

- Codex detects paste bursts. A single `write` of `text\r`, or `rex send` text
  followed by `send-key enter` about 48 ms later, leaves the text in the input
  box unsubmitted.
- A single `write` of `ESC[200~ text ESC[201~ \r` (bracketed paste, then CR)
  submits on Codex. `rex send` + 150 ms + Enter also submits.
- A failed submission leaves text in the box; the next bracketed paste merges
  with it and both are submitted. A failed typed delivery must be cleaned up.
- Claude submits `text\r` in one write and `send` + immediate Enter.
- Typing while an agent is `working` queues the text into the running turn on
  both agents.

## 3. Delivery channels

The agent's reported state chooses the channel:

| Agent state | Channel | Proof of delivery |
|---|---|---|
| `working`, between tool calls | `PostToolUse` hook adds a notice as `additionalContext` (Antigravity: `PreInvocation` injects an ephemeral step before the next model call) | The notice is in the model context |
| `working`, about to end its turn | `Stop` hook returns `decision: block` with the notice | Same; the agent keeps working |
| user submits a prompt | `UserPromptSubmit` hook adds the notice as `additionalContext` | Same |
| `idle` / `done` / `error` at its prompt | Injector types a doorbell (section 5) | Status changes to `working` after the write |
| `blocked` (question, permission, auth) | Nothing; wait | — |
| not running | Mail waits; the next `SessionStart` announces it | Same as hooks |
| Pi, any state | Its extension watches the inbox and calls `pi.sendMessage`: a turn of its own when idle, `steer` into a running turn; `agent_before_settle` continues a run that would end with mail unread | The notice is in the model context; nothing is typed |

Consequences:

- Text is typed only into an agent at rest. The race in the Herdr design, a
  doorbell arriving while the user types into a working agent, cannot happen.
- Mail that arrives during a turn is seen within that turn, without waiting for
  amq's retry ladder.
- A blocked agent is detected from its own report, not from the screen.

### Announcement ledger

Hooks and the injector share one per-handle ledger of message ids already
announced. A message id is the file name under
`<root>/agents/<handle>/inbox/new/` without `.md`.

- A hook announces only ids not yet in the ledger, then records them.
- `Stop` also blocks once when undrained mail exists and `stop_hook_active` is
  false, even if every id was announced earlier in the turn: the agent is about
  to stop with mail unread. With `stop_hook_active` true it blocks only for new
  ids. A turn therefore cannot loop on the same mail.
- A successful typed doorbell records the ids present at injection time.
- Ids no longer in `inbox/new` (drained) are pruned on every ledger write.

Notices contain counts and the drain command only, never message content: the
agent reads content through `amq drain`, which marks it as message data.

### Interplay with amq wake

One `amq wake` per handle watches the inbox and calls the injector. Its
`--retry-until injected` ladder (5 s doubling to 2 min) keeps re-calling the
injector while it reports `deferred`. While the agent is working the injector
defers, and the hooks deliver in-band; once the agent drains, amq sees inbox
progress and stops. If an agent ends its turn with mail it chose not to drain,
the next ladder step types a doorbell. Worst-case latency for that corner case is
the ladder cap (2 min); this is accepted.

## 4. Identity

**Handle = `<kind>-<role>-<NN>`, owned by the agent session.** Panes are
disposable; agent sessions are not. Claude Code and Codex keep their session id
across a resume (executed 2026-10-28: `claude -p --resume` and `codex exec
resume` returned the id of the session they resumed; a Codex hook's
`session_id` is the id in its rollout file name), so the handle is bound to that
id, not to the pane. Adoption, in order:

1. **Resume.** A record of the same agent session and kind takes its handle
   back, in this pane or any other, unless another pane's agent is running it.
   Peers' mail and the conversation's own references to the handle still
   reach it; mail sent while it was closed is waiting as unread. This also
   covers a process that switches session (`/resume` inside the agent); the
   session it left keeps its own handle.
2. **Same process.** This pane's live record of the same process keeps its
   handle: a repeated hook, a compaction, a new session in the process
   (`/clear`).
3. **New name.** `<kind>-<role>-<NN>`, or `<kind>-<NN>` without a role, with
   the lowest `NN` from `01` that is not taken. The role comes from the pane
   label a peer set when it launched the agent (`rex split --label reviewer
   -- codex --no-daemon` → `codex-reviewer-01`): lowercased, other characters
   folded to `-`, a leading kind and a trailing number dropped, so a label the
   adapter wrote earlier reads back as the same role. Rex's default label
   `shell` carries no role. The kind is always the agent's own.

Taken handles are every record's (live, or reserved for a departed session) and
every mailbox holding undrained mail, so a new agent never inherits someone
else's session or unread messages. A new session in a pane is a new identity:
it does not inherit the handle of the pane's previous session. The adapter
then sets the pane label to the handle, so it is visible in Rex. Session
labels are left alone; Rex's own skill renames the session after the task.

**Record** (one JSON file per handle, the single source of truth):

```json
{
  "handle": "codex-reviewer-01",
  "kind": "codex",
  "server": "unix:///…/rex/server.sock",
  "session_id": "session:…",
  "block_id": "block:…",
  "agent_pid": 13318,
  "agent_session_id": "01a1526a-…",
  "adopted_at": "2026-10-18T21:28:15Z",
  "departed_at": null
}
```

The record holds only facts the adapter owns. The waker's identity and
liveness belong to amq: the adapter asks `amq wake check` and retires a waker
by the block its saved target names, never by a copy of its generation. The
binding is `(server, block_id, agent_pid)`; block ids are never reused, so a
restarted Rex server makes old records' blocks unfindable and collection
parks them. A different pid in the same block is a different agent process: it
keeps the handle only by resuming the same session, and the record it
replaces is parked as that session's reservation.

Codex can run an ephemeral memory-consolidation session inside the same
terminal process. It inherits the Rex block but has no persisted transcript.
Hooks that explicitly send `transcript_path: null` are ignored before any
adoption, departure, or announcement-ledger effects, regardless of startup
order or the existing record. A different session ID alone is not evidence of
a background thread: interactive switches and missed startup hooks retain
normal behavior. An omitted transcript field preserves older-hook behavior.
This is separate from `--no-daemon`, which preserves the Rex environment but
does not isolate hook session ownership. A terminal hook that temporarily
cannot resolve its transcript is also ignored; the next hook with a valid
transcript resumes delivery. Existing mailbox and binding state are preserved.

Evidence for this filter: Codex's memory consolidation config sets
`ephemeral = true`, and `hook_transcript_path` returns no path without a
rollout (**located**, upstream `memories/write/src/phase2.rs` and
`core/src/session/mod.rs`). An independent Codex 0.164.0 probe captured 30
interactive hook payloads with non-empty transcript paths (**executed**,
2026-10-27). `/new` sends `source=startup` in the same process without ending
the old thread. Replaying those payloads catches the rejected session-ID/PID
heuristic's `/new` regression. The actual memory-worker null payload has not
been captured locally. The upstream reporter independently describes observing
a memory-worker `SessionStart` with `source=startup`, a different `session_id`,
and `transcript_path: null` in
[openai/codex#52442](https://github.com/openai/codex/issues/52442)
(**reported**, verified 2026-10-27). This corroborates the discriminator but
does not constitute a local live memory-worker test. Explicitly ephemeral
Codex runs are not adopted by this contract.

This filter is a workaround for that upstream bug. Remove it only after all
supported Codex versions suppress internal-worker hooks across the lifecycle,
or replace it with an upstream documented worker discriminator. Before removal
or replacement, verify that background start/tool/stop/end hooks cannot change
the terminal binding or announcement ledger, and that interactive startup,
`/new`, resume, clear, and fork retain normal delivery. An issue closure alone
is not sufficient evidence.

**Window-mates.** Agents on one task are placed side by side in a Rex window
(tab), so the window is where an agent's mates are; the session and the server
are left to `who`. Rex delivers no layout events, but the moment that matters,
an agent appearing, is an adoption, so adoption does the work under the
lifecycle lock, which also serialises agents that start together:

- One `rex block ls -s <session>` maps blocks to windows. The live records in
  the adopted block's window, other than this block, are its mates; the
  identity notice names them.
- An agent that just arrived in its block (a new handle, a session resumed
  in another block, or one taking its handle back after it departed) queues
  "`<handle>` joined your Rex tab" for each mate. A live record adopted again
  in its own block (a repeated start hook, a compaction) tells no one.
- Departure (`park`: SessionEnd, collection of a crash or a closed pane, a
  replacement in the block) queues "`<handle>` left your Rex tab" for each
  live agent recorded in the same window. The record keeps the window its
  block was in at adoption (`window_id`), because a crashed agent's block is
  already gone when collection finds it.
- A queued note is delivered once, at the mate's next prompt:
  `UserPromptSubmit` (Claude Code, Codex), Pi's `before_agent_start`, or
  Antigravity's next `PreInvocation`. Tool hooks, Stop, and Pi's mail watcher
  carry none: they would interrupt a turn or start one for advice that can
  wait. Antigravity is the accepted exception: it has no prompt hook, and
  `PreInvocation` runs before every model call, so a mate that joins while it
  works is announced mid-turn. Departure drops undelivered notes; a hook that
  fails before it replies leaves them queued.
- Mates are advice. When the window cannot be listed, adoption stands without
  them and the failure goes to the hook log. A block moved to another window
  is not noticed until the next adoption; `who` is always current.

Evidence (2026-10-29, **reproduced**): Claude, Codex, Antigravity, and Pi
started into one tab and a Claude control in another, each on this
implementation; every agent named exactly the mates and joins it was due, the
control none, and the queue ended empty.

**Crews and roles.** The window is the team; two labels say what kind of
team it is and what each member does. The tab label names a crew, the pane
label a role (the same `core.Role` that names the handle). An agent learns
both with its identity, at adoption:

- The crew's `team-conventions.md` (who leads, the order of handoffs, when to
  stop) and the role's prompt are looked up from the agent's directory (the
  block's foreground process): `.agents/crews` there or above, then
  `~/.config/rex-amq-adapter/crews`, then the built-in set. The first file
  found wins whole; a crew's own `<role>.md` beats the shared
  `roles/<role>.md`. Conventions win over a role prompt where they differ.
- A tab that names no crew (Rex's default `Window 82b616eb`, any unknown
  name) gives role prompts only, so a reviewer split into an ordinary tab is
  still briefed. An agent without a role in such a tab hears nothing more.
- Briefings are advice, like mates: a lookup that fails is logged and left
  out. A prompt over 4 000 bytes is named by path instead of inlined. A
  session re-adopted in its own block is not briefed again.
- Built in: roles `implementer`, `reviewer`, `qa`, `architect`, `writer`,
  `editor`; crews `pair`, `ship`, `dev`, `docs` (with its own `architect`),
  `design`, rewritten from Runner's examples (MIT, see
  `internal/crew/builtin/NOTICE.md`). The file layout follows Runner's, so a
  crew can move between the two once its conventions say who to message
  rather than how.
- `rex window new` cannot label the block and `rex run` cannot label the
  tab, so `rex-amq-adapter tab <crew> --role R -- CMD` opens both in one
  `session.new_window` call; `crews` lists what an agent in a directory
  would find.

Evidence (2026-10-29, **reproduced**): `tab pair --role implementer` opened a
Claude, and a Codex was split beside it with `--label reviewer`. Asked what
they had been told, the implementer named its role, the crew, its lead, and
its handoff; the reviewer quoted its role prompt and the conventions and
stood by.

**How an agent finds its identity.** `rex-amq-adapter env` resolves the record
from `REX_BLOCK` and prints `export AM_ROOT=… AM_ME=…`. Agents run
`eval "$(rex-amq-adapter env)"` before each `amq` command. No identity files.

## 5. Typed doorbell (injector)

`amq wake` runs
`rex-amq-adapter inject <server> <block> <handle> <root> <payload>` and reads
`AMQ_INJECT_PROGRESS=<accepted|deferred|failed>` from stderr. `deferred` exits
non-zero (amq treats a deferred marker with exit 0 as `uncertain`).

1. **Binding.** Load the record for `<block>`; the handle must match. The block
   must exist (rex exit 3 means gone) and its foreground pid must equal
   `agent_pid`. A mismatch marks the record dead and reports `failed`.
2. **State.** Read program status (root record):
   `working` → `deferred` (hooks deliver); `blocked` → `deferred`;
   `idle`/`done`/`error`/absent → continue.
3. **Input box.** `screen.Check(kind, capture vt)`: `typed` → `deferred`
   (`draft_in_box`); trust dialog → `deferred`; `empty` → continue.
4. **Write** one `write`: `ESC[200~` + notice + `ESC[201~` + `\r`.
5. **Confirm.** Poll program status every 150 ms for up to 3 s. Only a report
   of `working` is `accepted`: an agent at rest may refresh its status without
   starting, and no turn completes inside one poll interval. An agent that
   reported no status before the write is `accepted` once its box empties
   (the only evidence available). Otherwise capture the box: if it holds
   exactly our notice (`screen.BoxHolds`: the box only, whitespace ignored,
   never the transcript), clear it with `ctrl+u` and report `deferred`; if it
   holds anything else, leave it and report `deferred`. An unconfirmed write is
   never `failed`: amq does not retry a failed cohort until new mail arrives,
   and an idle agent fires no hook, so failing would strand the mail.
6. Record the outcome in `<state>/inject/<handle>.jsonl`; on `accepted`, record
   the announced ids in the ledger.

A doorbell deferred again and again is the one delivery failure that is
otherwise silent: mail waits durably, the sender waits for a reply, and
nothing reports that, say, an agent update changed its input box so every
attempt ends `box_not_found`. `who` therefore shows each agent's latest
outcome and how long the same outcome has repeated (`core.LastDelivery`), and
the agent guide tells a sender to read it before writing again. The log is cut
to its newest entries (at most 1000, half of 1 MB) once it passes 1 MB.

The notice is amq's payload plus
`(you are <handle> in Rex; first: eval "$(<abs path> env)")`, kept on one line.

amq runs the injector through a wrapper, `<state>/bin/inject`, which execs the
adapter binary and is rewritten whenever the binary's path changes. amq refuses
an `--inject-via` under a group- or world-writable directory, and Homebrew's
Cellar is group-writable: switching a real install to Homebrew left every
revived waker unstartable until this wrapper existed (executed). The wrapper's
path also stays put when the binary moves, so a `brew upgrade` or a switch from
`go install` replaces no waker.

The rex binary is resolved at run time (`$REX_AMQ_REX`, then `PATH`, then the
newest `~/Library/Application Support/com.superlogical.rex/RexServer/Versions/*/…/rex`)
because Rex installs it under a versioned path; it is never part of the waker's
saved target. The server endpoint is an injector argument because the waker's
environment is not guaranteed to carry `REX_SERVER`.

`amq wake` is started with `--inject-timeout 10s`; the injector's own deadline
is 8 s.

## 6. Lifecycle

All entry points are agent hooks, installed as
`'<abs>/rex-amq-adapter' hook <event> --agent <kind> 2>/dev/null || true`
with a timeout (both harnesses run hook commands through a shell). Outside Rex
(`REX_BLOCK` unset) every hook exits 0 silently. Hook failures are appended to
`<state>/logs/hooks.log`, since stderr is discarded.

| Hook | Action | Installed timeout / adapter budget |
|---|---|---|
| `SessionStart` | Garbage-collect, adopt this block (handle, mailbox, record, waker), print identity and unread count as context | 5 s / 4.5 s |
| `UserPromptSubmit`, `PostToolUse` | Announce new ids from the ledger; filesystem only, no subprocess (measured p50 6.8 ms, max 16.7 ms over 30 runs) | 3 s / 2.5 s |
| `Stop` | Block once per section 3, then supervise the waker: one `amq wake check` (about 60 ms), and `Ensure` when it is dead or runs an older binary | 3 s / 2.5 s (supervision bounded to 1.5 s) |
| `SessionEnd` | Retire the block's waker (identified by amq's saved target), set `departed_at` | 3 s / 2.5 s (Codex clamps to 3 s) |

**Antigravity** has no `SessionEnd` hook and no `stop_hook_active`. Its agents
depart through collection, and its Stop hook blocks only for mail never
announced (the adapter treats every agy stop as already continued), so a turn
cannot loop. A Stop the user cancelled, or one caused by an error or a loop
limit, is never overridden. Every agy hook prints a JSON object, `{}` when it
has nothing to say. Its peers start with `agy -i "<brief>"`.

**Pi** has no command hooks. `setup` installs a small JavaScript extension
(`~/.pi/agent/extensions/rex-amq-adapter.js`, or under `PI_CODING_AGENT_DIR`)
that holds no policy: it runs `rex-amq-adapter hook <event> --agent pi` with a
JSON payload (`session_id`, `agent_pid` = `process.pid`, `stop_hook_active`) and
reads `{"notice", "inbox"}` back. Events: `session-start` (adopt; returns the
inbox path), `mail` (the watcher saw mail: announce new ids), `settle`
(`agent_before_settle`: the Stop policy, with the extension's per-run flag as
`stop_hook_active`), and `session-end` (only when `session_shutdown.reason` is
`quit`; reload, new, resume, and fork keep the process and pane). Pi is the
only agent the extension wakes: adopting Pi starts no waker and retires any
waker an earlier agent left in the block, so nothing is ever typed into it.
At session start existing mail is not marked announced; the watcher's first
scan delivers it with a turn of its own. The watcher is armed before that scan,
debounced, and backed by a rescan every 30 s, since `fs.watch` on macOS can
coalesce or drop events. Its peers start with `pi "<brief>"`.

**Lazy adoption.** `UserPromptSubmit`, `PostToolUse`, and `Stop` adopt the
agent first when its block has no live record, and add the identity notice to
their output (a `Stop` hook never blocks only to state the identity). This is
the common path for Codex: its hooks run only after the user approves them,
which usually happens after the session started, and in the e2e runs the
adapter's `SessionStart` did not adopt Codex at startup. It also covers an
adapter installed mid-session, and heals a record that collection parked by
mistake. Every adoption collects first, so a resumed session finds the record
of the pane it left already parked.

**Adopt** (under the state lifecycle lock): resolve the block's foreground
process and confirm it is the agent; choose the handle; `amq init --force` with
the merged agent list under the root registry lock (port from herdr); ensure the
waker with herdr's `check` → keep / start / repair / replace decision; write the
record only after `amq wake check` reports the expected target live.

**Garbage collection** runs before every adoption, on `rex-amq-adapter who`,
and on `rex-amq-adapter collect`: a live record whose block is gone or now runs
a different foreground process is parked like a departure (waker retired,
`departed_at` set); a departed record older than 7 days is deleted with its
ledger and inject log, so the handle can be named afresh. A server that cannot
be reached leaves its records alone, since absence cannot be proven. The
departed record itself is the reservation: while it exists its handle is taken
for new names, and its session gets the handle back on resume. Past 7 days a
resumed session is named afresh, and peers that still address the old handle
find it missing from `who`. Mailboxes are kept; collection of old mailboxes is
out of scope for the first version.

The injector never retires its own waker. A waker whose agent is gone keeps
reporting `failed` until the next `SessionStart` collects it; the cost is one
idle process per departed agent until then.

**Waker supervision.** `amq wake` has no supervisor, and nothing in Rex
reports its death. Every `Stop` (the moment an agent starts depending on its
waker) checks it and brings back a dead one; an independent review killed a
waker, delivered mail to the idle agent, and saw nothing ring until this was
added (executed). A waker that dies mid-turn is revived at that turn's end; one
that dies while the agent sits idle for a long time is revived at the agent's
next turn or session start. The hook timeouts and the adapter's budgets come
from one table (`install.hookTimeouts`).

**The injector depends on no environment.** A dead waker revived with
`amq wake repair` runs the injector in amq's own environment, which carries
neither `REX_AMQ_ADAPTER_HOME` nor anything else the adapter set; the first
end-to-end run of supervision failed `not_adopted` for that reason (executed).
The injector therefore derives the state directory from the root amq passes it
(`store.PathsForRoot`). After a `failed` outcome amq holds that waker's next
attempts for roughly 30–50 s (measured), so a spurious `failed` costs a
delivery delay, not mail. `amq wake repair` does not carry `--no-self-upgrade`
over, so a repaired waker may self-upgrade its amq image.

## 7. Installation

`rex-amq-adapter setup|status|remove [--agent claude|codex] [--dry-run] [--json]`,
modeled on `rex skill`:

- **Skill**: a stub `SKILL.md` in `~/.claude/skills/rex-amq-adapter` and
  `~/.agents/skills/rex-amq-adapter` whose body runs
  `rex-amq-adapter __internal skill`. The guide ships inside the binary, so an
  upgrade updates what agents read.
- **Pi**: the extension file carries a marker line like the skill stub, so a
  same-named extension without it is foreign: kept by setup (unless `--force`)
  and by remove. Pi reads `~/.agents/skills`, which it shares with Codex; the
  skill is installed once for both.
- **Antigravity**: the adapter owns the hook group named `rex-amq-adapter` in
  `~/.gemini/config/hooks.json`; setup replaces that group in place or appends
  it, removal deletes it, and no other group is touched. The skill stub goes to
  `~/.gemini/config/skills/rex-amq-adapter`.
- **Hooks**: appended to `~/.claude/settings.json` and `~/.codex/hooks.json`.
  Existing hooks and keys are preserved. Entries are identified by their
  command string. New entries go at the end of each event's array: Codex trusts
  hooks by `<file>:<event>:<group index>:<hook index>` plus content hash in
  `~/.codex/config.toml`, so inserting earlier would invalidate the trust of
  every hook after it (executed).
- **Status vocabulary**: `current`, `outdated`, `missing`, `foreign`.
  A foreign skill of the same name is reported as a conflict and kept unless
  `--force`. `remove` deletes only what setup installed.
- Codex requires approving new hooks in `/hooks`; setup prints this.
- The agent guide carries every Rex command it uses (opening a peer in a pane,
  tab, or session; reading a peer's screen and status). It does not depend on
  Rex's own skill, so an agent needs one skill lookup, not two.

## 8. Paths

Overridable with `REX_AMQ_ADAPTER_HOME` (tests use a temporary directory).

```
~/.local/state/rex-amq-adapter/
├── amq-root/                 shared AMQ root (agents/, meta/config.json)
├── bin/inject                the wrapper amq wake runs as --inject-via
├── records/<handle>.json     one record per handle (earlier versions named it by block)
├── ledger/<handle>.json      announced message ids
├── inject/<handle>.jsonl     injector outcomes
├── logs/<handle>.log         amq wake stdout/stderr
├── logs/hooks.log            hook failures
└── lifecycle.lock
```

## 9. Module layout

```
cmd/rex-amq-adapter/   subcommands: hook, inject, env, who, collect, crews, tab, setup, status, remove, __internal, version
internal/app/          entry points: observe, ask core, act (adopt, inject, announce, depart, collect)
internal/core/         pure policy: handle choice, inject decision, announce decision, window-mates, notice text
internal/crew/         crew and role prompt lookup across project, user, and built-in layers; built-in prompts
internal/fsutil/       flock and atomic replace
internal/rex/          rex CLI client: process, program_status, capture, write, labels, whoami
internal/amq/          wake check/start/repair/retire, mailbox registration, inbox listing (ported)
internal/screen/       input-box classifier (ported from herdr-amq-adapter)
internal/store/        records, ledger, inject log, reservations, locks
internal/install/      settings.json / hooks.json merge, stub skills
internal/guide/        embedded agent guide
```

The core takes observations and returns decisions; `rex/`, `amq/`, and `store/`
are thin shells. Tests cover the core directly and the shells against fakes and
temporary directories. An end-to-end script drives real agents in scratch Rex
tabs.

Connecting a further agent follows [adding-an-agent-kind.md](adding-an-agent-kind.md).

## 10. Out of scope (first version)

- OpenCode.
- Cross-machine delivery. Rex can reach other servers (`rex -S host`); whether
  that can replace amq-bridge is a separate investigation.
- Mailbox archival for departed handles.
- Pi has no amq retry ladder. Mail announced to Pi but left undrained is
  reminded when the run ends (`agent_before_settle`), and again every 5 minutes
  while Pi sits idle (`core.RemindAfter`, from the extension's 30 s rescan);
  the `amq wake` agents get amq's ladder (up to 2 min between doorbells)
  instead.
- A status view. A `rex.action` in `init.lua` can open one later; Rex already
  shows blocked/done indicators from program status.

## 11. End-to-end evidence

Run on 2026-10-18 with real Claude Code (Haiku) and Codex in scratch Rex tabs,
state in a temporary `REX_AMQ_ADAPTER_HOME`, Claude hooks from a `--settings`
file written by `setup`, Codex hooks passed inline with `-c`.

| Scenario | Observed |
|---|---|
| Claude `SessionStart` | adopted as `claude`, pane relabeled, waker live with the expected target |
| Codex first prompt after approving hooks | adopted lazily as `codex`; the agent named its handle from the injected identity notice |
| Claude sends to idle Codex, Codex replies | both doorbells typed and `accepted`; Codex drained and replied; Claude drained the reply |
| Mail while Codex runs tools | injector `deferred` ×3 (`agent_working`), nothing typed; `PostToolUse` announced; Codex drained mid-turn |
| Mail while Claude writes without tools | `Stop` blocked once; Claude drained before ending the turn |
| Mail while a user draft sits in Codex's box | `deferred` ×2 (`draft_in_box`), draft untouched; `accepted` on the next retry after the box was cleared |
| Codex `/quit` | `SessionEnd` parked the record; waker `missing` |
| Claude killed with SIGKILL | record stayed live until `collect`, which parked it and retired the waker |
| New Codex while `codex` is reserved | adopted as `codex-2` |
| Antigravity in a scratch project with `.agents/hooks.json` from `setup` | adopted as `agy` at its first turn, pane relabeled; it named its handle from the injected identity |
| Mail to idle Antigravity | doorbell `accepted`; agy asked permission to run the drain (program status `blocked kind=permission`), then drained and replied |
| Mail while Antigravity runs tools (launched with `-i "<brief>"`) | injector `deferred` ×3; `PreInvocation` announced; agy drained mid-turn |
| Mail while Antigravity writes without tools | Stop continued the turn; agy drained before ending it |
| Pi started with `pi -e <extension from setup>` | adopted as `pi` at startup without a prompt; `agent_pid` equals Rex's foreground pid; no `amq wake` process |
| Mail to idle Pi | the extension started a turn; Pi drained and replied; no injector run |
| Mail while Pi runs `sleep 10` | delivered by `steer` into the running turn; Pi drained it and finished its task |
| Mail announced but left undrained when a run ends | `agent_before_settle` continued the run with the settle notice; Pi drained |
| Pi `/reload`, then mail | record stayed live; the re-armed watcher delivered |
| Pi `/quit` | record departed |
| Mail to idle Pi with an unsent draft in its editor | delivered with a turn of its own; the draft stayed in the editor |

## 12. Pre-mortem

| Failure | Guard |
|---|---|
| A hook slows every tool call | `PostToolUse`/`Stop`/`UserPromptSubmit` do no subprocess work; latency measured in e2e |
| A hook error breaks the agent | Wrapped in `2>/dev/null \|\| true`; outside Rex it exits silently |
| `Stop` loops forever | Blocks for already-announced mail only when `stop_hook_active` is false |
| Doorbell merges with a user's draft | Typed only at rest, after an `empty` box check, and confirmed; leftover text is cleared only when it is exactly ours |
| Codex swallows Enter | Bracketed paste + CR in one write (executed) |
| Rex upgrade moves the rex binary | Resolved at run time, never baked into the waker target |
| A hook in one Rex session sees every agent in another as gone | rex resolves `-b` within `$REX_SESSION`, which hooks inherit; collection then parked live agents, whose next hook re-adopted them and spawned a fresh waker, over and over. Every rex call names its target by id, so the client drops `REX_SESSION` (executed: old and new `collect` on a copy of real state) |
| A parked record is re-adopted by its own still-running session, and the agent is told its identity again | The identity notice goes only to a session that did not hold the handle before. Pi's mail watcher turns any notice into a turn of its own, so a repeated identity cost Pi a whole turn (reproduced; executed in `resume_test.go`) |
| A new agent inherits stale mail | New names skip mailboxes with undrained mail and reserved handles |
| A resumed agent loses its handle, and its peer writes to nobody | The handle belongs to the agent session, not the pane; collection runs before adoption so the closed pane's record is parked and claimable; reservations last 7 days (executed in `resume_test.go`) |
| A handle names the wrong harness (`codex-…` running Claude) | The kind is never read from a label; a pane that changes kind keeps only its role |
| Two agents claim one handle | Adoption holds the lifecycle lock; mailbox registration holds the root registry lock |
| Installing hooks invalidates Codex trust | Append-only placement |
| A waker dies and an idle agent never hears of mail | Every `Stop` checks the waker and revives a dead one; nothing else in Rex would notice (executed by the independent review, fixed and tested) |
| Pi's notice is lost after the ledger marks it announced | The ledger is written before `pi.sendMessage` returns; if delivery fails there, an idle Pi is reminded after 5 minutes, and `agent_before_settle` reminds at the end of the next run. While a Pi dialog is open (`ui_prompt_start` … `ui_prompt_end`) the extension asks the adapter nothing, so no mail is marked announced; it delivers when the dialog closes (not yet exercised end to end) |
