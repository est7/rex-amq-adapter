# Claude Code background sessions: which pane shows a conversation?

Probed 2026-11-05 on macOS with Claude Code 2.1.318 and Rex API revision 10.
Evidence levels: **observed** (a probe on this machine, read back),
**documented** (code.claude.com docs, quoted by URL), **inferred**.

## The incident

A conversation the user was typing into, in pane `01a1aa7b-42de` of Rex session
`diagnose-upload-speed`, was adopted as `claude-73`. That handle belonged to a
DiaTweaks conversation in pane `01a1a9a3-747c` of another session. Its `rex
split -b "$REX_BLOCK"` opened a reviewer beside the DiaTweaks pane. Mail to
`claude-73` rang the DiaTweaks pane: four doorbells were `accepted
box_emptied` (observed: `inject/claude-73` log).

## Why: the conversation ran in Claude Code's supervisor

Agent view and background sessions are documented: "Each background session is
a full Claude Code conversation that keeps running without a terminal
attached"; "The supervisor is a background service that runs your background
sessions … Claude Code starts it the first time you background a session or
open agent view" (documented, https://code.claude.com/docs/en/agent-view.md).
A session becomes one by `/bg`, `claude --bg`, dispatch from agent view, or `←`
on an empty prompt (same page; changelog 2.1.196).

| Fact | Evidence |
|---|---|
| Process chain of the conversation: `bash ← claude bg-spare (3340) ← claude bg-pty-host ← claude daemon run --origin transient (12303) ← claude (78802, the DiaTweaks pane's TUI)` | observed (`ps`) |
| The supervisor was started by the first TUI that needed it, and every job it hosts has that TUI's environment: job 3340 has `REX_BLOCK=01a1a9a3-747c…` and `REX_SESSION` of the DiaTweaks pane | observed (`ps -E`) |
| Each TUI's own environment names its own pane: 96315 → `01a1aa7b-42de`, 78802 → `01a1a9a3-747c`, 12454 → `01a1aac3-802e` | observed (`ps -E` against `rex block call process`) |
| The job's hooks and tool shells get `CLAUDE_PID` = the job's process (3340), not the TUI (96315), and `CLAUDE_CODE_SESSION_ID` = the job's session | observed (tool shell env) |
| Rex resolves `-b` within `$REX_SESSION`, so with the inherited session every block of the user's real session reads as "not found" | observed |
| The docs do not say which environment a job inherits, and no hook input field identifies the attached terminal | documented (agent-view.md, hooks.md: common fields only) |

So `REX_BLOCK` and `REX_SESSION` cannot be trusted in a background job. This is
the same failure as Codex's shared app-server daemon (`docs/design.md` §2), now
in Claude Code.

## What maps a job to the pane showing it

| Signal | What it says | Status |
|---|---|---|
| `~/.claude/sessions/<pid>.json` of a job | `kind: "bg"`, `jobId` (the session id's first 8 characters) | observed; undocumented |
| `~/.claude/sessions/<pid>.json` of a TUI that backgrounded its own conversation and still shows it | `kind: "interactive"`, `parkedJobId: <jobId>`: 96315 → `407cc156`, 78802 → `5947a017`, 12454 → `3bc37f31`, one to one | observed; undocumented |
| A pane running `claude attach <id>` | the job id is in its argv; it has **no** registry entry and its pane title is empty | observed (probe job `5c930d67`) |
| A TUI's terminal title | the job's name (`◐ Adapter spike brief for claude-89`): a name, not an id | observed |
| `claude agents --json` | lists jobs and unparked interactive TUIs; no attachment field; parked TUIs are absent | observed |
| `~/.claude/daemon/roster.json` | `supervisorPid`, and per job its worker pid, `rendezvousSock`, `ptySock`; no client | observed; the file is documented as "List of running background sessions" |
| `~/.claude/jobs/<id>/state.json` | state, `interactiveLineage`, `firstTerminalAt`; no client | observed; documented as internal ("read it via `claude agents --json`") |
| `lsof` on the sockets | every client (TUI, `claude attach`) and every job connects to the supervisor; the supervisor relays. No socket joins a client to a job directly | observed |
| `/status` "Session kind: background job · attached \| unattended" | whether some terminal is attached, not which | documented (changelog 2.1.221); not readable from a hook |

## Agent view: what a pane shows can change, and only its title follows

Probe (observed, 2026-11-05): two jobs J1 and J2 dispatched with `claude --bg`;
an interactive TUI (pid 22001) in a scratch Rex pane, driven with `rex
send-key`.

| Step | Registry of the TUI | Pane title (`rex block call title`) |
|---|---|---|
| started | `interactive`, no `parkedJobId` | (default) |
| `←` on the empty prompt | its own conversation became job `0b2188f4` ("Your conversation moved to the background"); `parkedJobId: 0b2188f4` | `2 awaiting input · claude agents` |
| Enter on J1 in agent view | still `parkedJobId: 0b2188f4` | `probe job j1` (J1's name) |
| `←` again (detach) | unchanged | `2 awaiting input · claude agents` |
| Enter on J2 | still `parkedJobId: 0b2188f4` | `probe job j2 ok2` |
| J2 renamed through its messaging socket (`{"type":"control","action":"rename",…}`) | the job's registry `name` changed, `nameSource: "peer"` | `✳ claude-probe-07 \| probe task`, at once |

So `parkedJobId` is the conversation the TUI moved to the background, not the
one it shows: after any switch in agent view it is wrong. The pane's terminal
title is the name of the job it shows, with a state glyph in front, and agent
view has a title of its own; it changes as the pane switches. A job's name can
be set by the adapter (`sessionTitle` from a hook, which sets the custom title),
so a name that carries the handle makes the pane showing the job findable by
title, uniquely. Codex has an agent view too (the user reports `←` opens it), and
agent-post finds a Codex daemon pane by its title as well.

Not probed: two terminals attached to one job; `terminalTitleFromRename: false`,
which keeps the custom title out of the terminal title.

## Codex, for comparison

Codex's daemon has the same inheritance. agent-post found a pane from the
TUI's terminal title holding the thread id's first 29 characters
(`tui.terminal_title` with `thread-id`), and that the app-server's `codex_tui`
origin names the TUI that started a thread, not the one showing it
(`~/EstProjects/agent-post/docs/research/codex-pane.md`).

## Lineage across `←`

agy's design review read in the binary that moving a conversation to the
background copies the parent's session id into the job's transcript. On this
machine it did not: job `407cc156`'s transcript (7.7 MB, 2242 entries) holds
only its own id, none of the parent `dcde68d2`, and its `state.json` has
`interactiveLineage: true` with no parent id (observed). So a job cannot take
the terminal session's handle back by lineage.

## What the adapter does (implemented, `docs/design.md` §4)

One rule: a pane shows a Claude agent when its terminal title, without its
state glyph, names it: its pane label once adopted, or a job's current name.
`parkedJobId`, `claude attach` argv, and sockets are not used. A job is placed
by title on hooks that run once a turn; a terminal session whose pane shows
agent view has left it; a doorbell is typed only into a pane that names its
agent; a job's commands take their identity from `CLAUDE_CODE_SESSION_ID`.
`CLAUDE_CODE_DISABLE_AGENT_VIEW=1` (or `disableAgentView: true`) turns off
background sessions and the supervisor (documented, env-vars.md).
