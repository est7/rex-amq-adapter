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

**Handle = block label.** A block whose label is not `shell` is named by the
user or by a peer that launched it (`rex split --label reviewer -- codex --no-daemon`); a
valid label is used verbatim. A `shell` block gets `<kind>` or `<kind>-N`
(`claude`, `codex-2`), and the adapter sets the block label to it so the handle
is visible in Rex. Session labels are left alone; Rex's own skill renames the
session after the task.

Automatic names skip handles that are taken by a live record, reserved, or whose
mailbox holds undrained mail, so a new agent never inherits someone else's
unread messages. An explicit label is the user's choice and is used even if
mail is waiting.

**Record** (one JSON file per block, the single source of truth):

```json
{
  "handle": "codex-2",
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
parks them. A different pid in the
same block is a different agent process; it keeps the handle (sticky per block)
and gets a fresh binding.

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
mistake.

**Adopt** (under the state lifecycle lock): resolve the block's foreground
process and confirm it is the agent; choose the handle; `amq init --force` with
the merged agent list under the root registry lock (port from herdr); ensure the
waker with herdr's `check` → keep / start / repair / replace decision; write the
record only after `amq wake check` reports the expected target live.

**Garbage collection** runs at every `SessionStart` and on `rex-amq-adapter
collect`: a live record whose block is gone or now runs a different foreground
process is parked like a departure (waker retired, `departed_at` set); a
departed record older than 24 h is deleted. A server that cannot be reached
leaves its records alone, since absence cannot be proven. The departed record
itself is the reservation: while it exists its handle is encumbered for
automatic names, and its block gets the handle back. Mailboxes are kept;
collection of old mailboxes is out of scope for the first version.

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
├── records/<block>.json      one record per adopted block
├── ledger/<handle>.json      announced message ids
├── inject/<handle>.jsonl     injector outcomes
├── logs/<handle>.log         amq wake stdout/stderr
├── logs/hooks.log            hook failures
└── lifecycle.lock
```

## 9. Module layout

```
cmd/rex-amq-adapter/   subcommands: hook, inject, env, who, collect, setup, status, remove, __internal, version
internal/app/          entry points: observe, ask core, act (adopt, inject, announce, depart, collect)
internal/core/         pure policy: handle choice, inject decision, announce decision, notice text
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
| A new agent inherits stale mail | Automatic names skip mailboxes with undrained mail and reserved handles |
| Two agents claim one handle | Adoption holds the lifecycle lock; mailbox registration holds the root registry lock |
| Installing hooks invalidates Codex trust | Append-only placement |
| A waker dies and an idle agent never hears of mail | Every `Stop` checks the waker and revives a dead one; nothing else in Rex would notice (executed by the independent review, fixed and tested) |
| Pi's notice is lost after the ledger marks it announced | The ledger is written before `pi.sendMessage` returns; if delivery fails there, an idle Pi is reminded after 5 minutes, and `agent_before_settle` reminds at the end of the next run. While a Pi dialog is open (`ui_prompt_start` … `ui_prompt_end`) the extension asks the adapter nothing, so no mail is marked announced; it delivers when the dialog closes (not yet exercised end to end) |
