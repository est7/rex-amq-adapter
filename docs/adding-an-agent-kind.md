# Adding an agent kind

The procedure for connecting a new coding agent (a "kind": `claude`, `codex`,
`agy`, `pi` today) to rex-amq-adapter. Follow it in order. Every claim you rely
on must come from a probe you ran or the agent's own documentation; record each
in `docs/design.md` §2 with its evidence level (executed / located / asserted).

Read `docs/design.md` first: this document assumes its delivery channels
(§3), identity (§4), injector (§5), and lifecycle (§6).

## 1. What the adapter needs from an agent

The adapter performs five duties for every agent. Map each onto something the
new agent provides; a duty with no mapping is a gap to document, not to paper
over.

| Duty | Purpose | Provided today by |
|---|---|---|
| **Adopt** | Give the agent a handle, mailbox, record, and (unless in-process) a waker; tell it its identity | `SessionStart` hook; lazily by the first other hook (`announce` in `internal/app/hook.go`) |
| **Announce in-band** | Tell a *working* agent about new mail without typing into its terminal | Claude/Codex `PostToolUse` + `UserPromptSubmit`; agy `PreInvocation`; Pi extension `sendMessage` |
| **Hold the turn end** | Keep an agent that is about to stop with unread mail going, exactly once (loop guard) | Claude/Codex `Stop` + `stop_hook_active`; agy `Stop` (`decision: continue`, guard emulated in `normalize`); Pi `agent_before_settle` |
| **Wake at rest** | Reach an *idle* agent | typed doorbell via `amq wake --inject-via` (needs OSC 7501 status + an input-box classifier); Pi: `sendMessage(…, {triggerTurn: true})` |
| **Depart and supervise** | Retire the waker when the agent ends; revive a dead waker | `SessionEnd`; `collect` when there is none (agy); `Stop` runs `SuperviseWaker` |

## 2. Probe the agent

Run every probe in a scratch Rex tab opened with `--focus=false`, with
isolated adapter state (`REX_AMQ_ADAPTER_HOME=<temp dir>`) and isolated agent
configuration (section 5). Save raw outputs; they become fixtures and evidence.

### 2.1 Process and status

```bash
rex window new -s "$REX_SESSION" probe-<kind> -c <dir> --focus=false --json -- <agent>
rex block call -b <block> process          # foreground .name: what the adapter will see
rex block call -b <block> program_status   # OSC 7501 root record
```

Record, by driving the agent through each state (an idle prompt, a turn with a
`sleep` tool call, a question or permission dialog, a cancelled turn):

- the `app` value (`claude-code`, `codex`, `antigravity`, `pi`);
- the state at rest after a turn (`done` or `idle`), during a turn (`working`),
  and during a dialog (`blocked`, with or without `kind`);
- whether the agent reports status at all. Without OSC 7501 the injector
  falls back to the screen (`Confirmation.Reports == false`); expect weaker
  guarantees and say so in the design.

### 2.2 Input box (only for kinds woken by typed doorbell)

Capture VT screens with `rex capture -b <block> --format=vt`:

- the empty box, including any dim placeholder text;
- a draft typed with `rex send` (no Enter);
- a draft longer than the pane width (open the agent in a narrow split, e.g.
  `--ratio 35`), so the wrapped layout is known;
- a question or permission dialog;
- the box after `rex send-key -b <block> ctrl+u` (must be empty again).

Then establish the submit recipe: one `rex block call -b <block> write`
of `ESC[200~` + text + `ESC[201~` + `\r` must submit and flip status to
`working`. Also try `text\r` in one write and `rex send` followed by an
immediate `send-key enter`: Codex swallows that Enter (paste-burst detection),
so never assume a plain write works.

### 2.3 Hooks

From the agent's documentation (installed docs, `strings` on the binary,
official pages), find and then exercise:

- where user-level hook configuration lives and its structure (flat event
  arrays, named groups, plugin files);
- which events exist; find the equivalents of: session start, before or after
  each tool call / model call, user prompt submit, turn end, session end;
- the hook **input**: field names (snake_case vs camelCase), the session id,
  a "this stop was already continued" flag, a stop reason (cancel, error);
- the hook **output** that (a) adds text to the model context, (b) keeps a
  turn from ending with a reason, (c) means "nothing" (some agents require
  `{}`); prove each with a probe hook that prints a marker word and watch the
  agent repeat it;
- timeouts, their unit, and clamps (Codex clamps `SessionEnd` to 3 s);
- approval or trust: does the agent run new hooks only after the user approves
  them, and how is trust keyed (Codex: position + content hash, so install
  append-only);
- **when the first hook fires**: at process start, or only at the first turn.
  If only at the first turn, peers must be launched with their brief as the
  first prompt, and the launch flag goes into the guide;
- the environment hooks and tool shells inherit (`REX_BLOCK` must reach both).
  Probe any shared-background-server mode separately: the terminal frontend's
  environment does not prove that the backend inherits it. Codex requires
  `--no-daemon` for this integration.

### 2.4 In-process alternative

If the agent has an extension API that can add a message to the conversation
and start a turn while idle (Pi's `pi.sendMessage(…, {triggerTurn: true})`),
prefer it over typing: no screen classifier, no draft race. Note its event
names for session start / shutdown (and the shutdown reasons that do *not* end
the process), run start / settle, and dialog open / close.

### 2.5 Launch and skill

- the flag that starts the agent with an initial prompt (`claude "<brief>"`,
  `codex --no-daemon "<brief>"`, `pi "<brief>"`, `agy -i "<brief>"`);
- the user-level skill directory the agent reads, and whether it is shared
  with another kind (Pi and Codex share `~/.agents/skills`).

## 3. Choose the integration shape

| The agent has | Shape | Example |
|---|---|---|
| Command hooks with context injection and a turn-end hold | Hooks + amq wake waker + typed doorbell | Claude Code, Codex, Antigravity |
| An in-process extension API that can start a turn | Extension that calls the binary; `Kind.InProcess()`; no waker | Pi |
| Command hooks without context injection | Hooks for adopt/depart only; rely on the typed doorbell for every delivery; document the latency | none yet |
| Neither | Not supported: an agent the adapter cannot adopt cannot receive mail | — |

## 4. Change the code

Make the changes in this order; each step has tests before the next.

1. **Kind** — `internal/core/identity.go`: add the constant, accept it in
   `ParseKind`, and include it in `InProcess` if section 3 chose in-process.
   Extend `TestParseKind`.
2. **Input box** (typed-doorbell kinds) — `internal/screen/promptbox.go`,
   `inputBox`: add a case. Reuse `ruledBox` when the layout is a glyph under a
   rule (Claude, agy); follow the Codex case when wrapped rows share a
   background. Add the captures from 2.2 to `internal/screen/testdata/` as
   `rex-<kind>-*.ansi` (replace account emails, user names, and home paths with
   equal-length neutral text) and assert empty, typed, `BoxHolds` on the
   wrapped draft, and the cleared box in `internal/screen/rex_test.go`.
3. **Hook input** — `internal/app/env.go` `HookInput` and
   `internal/app/hook.go` `normalize`: map the agent's field names onto
   `SessionID` and `StopHookActive`. Without a loop flag, set
   `StopHookActive = true` (Stop then blocks only for never-announced mail, as
   for agy). Extend `HookInput.NormalStop` if its stop reasons differ.
4. **Events** — if the agent's in-band event is new, add an `Event…` constant
   in `internal/app/hook.go`, route it in `Hook`, and add its timeout to
   `hookTimeouts` in `internal/install/install.go` (the adapter's budget is
   derived from it by `install.Budget`; never restate a timeout elsewhere).
5. **Output** — `internal/app/hook.go` `Render`: one case per (kind, event)
   for the formats proved in 2.3. Test every shape in
   `internal/app/app_test.go` the way `TestAgyHookOutputShapes` does, including
   the "nothing to say" output.
6. **Installer** — `internal/install/install.go`: add the agent to `Agents`
   (paths honouring the agent's config-dir variable, `Found`), and its hook
   writer: append-only entries in an event array (Claude, Codex), an owned
   named group (agy), or a marked file (Pi's extension). `status`, `setup`,
   `remove` must stay idempotent, keep other entries and key order, never
   overwrite a foreign item without `--force`, and remove only what setup
   wrote. Test like `TestAgyHookGroup` / `TestPiExtension`.
7. **Guide** — `internal/guide/skill.md`: name the kind in the first
   paragraph and add its launch-with-brief form to "Start a peer".
8. **Docs** — `docs/design.md` §2 (primitives with evidence), §6 (lifecycle
   notes for the kind), §11 (end-to-end rows); `README.md` (supported agents,
   tested version, install notes such as approvals).

## 5. Verify end to end

Isolate both the adapter and the agent; never use the owner's real agent
configuration for a probe:

| Agent | How its hooks were isolated |
|---|---|
| Claude Code | `claude --settings <file written by setup in a fake HOME>` |
| Codex | hooks passed inline: `codex --no-daemon -c 'hooks.<Event>=[{hooks=[{type="command",command="…",timeout=N}]}]'` (approve them in the review screen) |
| Antigravity | a scratch git project whose `.agents/hooks.json` is copied from a fake-HOME `setup` |
| Pi | `pi -e <extension written by setup with PI_CODING_AGENT_DIR=<temp>>` |

Always export `REX_AMQ_ADAPTER_HOME=<temp dir>` for the agent you start; a probe
without it writes into the owner's real state directory.

Run every row; each must be observed, not inferred:

| Scenario | Expected |
|---|---|
| Agent starts (or its first turn starts) | `rex-amq-adapter who` lists it `live`; pane relabeled; it can name its handle |
| Mail to the idle agent | doorbell `accepted` (or in-process delivery), the agent drains and replies |
| Mail while it runs a `sleep` tool | injector `deferred agent_working`, nothing typed; in-band hook announces; drained mid-turn |
| Mail while it writes without tools | turn-end hold continues the turn once; drained |
| Mail while a draft sits in its box | `deferred draft_in_box`, draft untouched; delivered after the box is cleared |
| Mail while a dialog is open | `deferred agent_blocked` (or held by the extension); dialog untouched |
| Normal exit | record `departed`, waker `missing` |
| Kill the agent | `collect` parks it and retires the waker |
| Kill its waker, finish a turn, then send mail | waker revived by `Stop`; doorbell delivered |
| Peer launched with its brief and a `--label` | adopted under the label; reports back |
| Hook latency | in-band hook well under its budget (measure p50/max) |

Clean up before reporting: close every tab you opened, confirm no `amq wake`
you started survives (`pgrep -fl <temp dir>`), and remove any trust entry the
agent recorded for a temporary path (Codex `~/.codex/config.toml`
`[hooks.state."…"]` and `[projects."…"]`; Antigravity
`~/.gemini/antigravity-cli/settings.json` trusted folders), backing the file up
first. Report residue you could not remove.

## 6. Lessons that cost a debugging session

- **Status is the gate, the screen is the fallback.** Read OSC 7501 before the
  box; a dialog often defeats the classifier (`unknown`) but reports `blocked`.
- **`working` is the only proof of submission.** Agents refresh status at rest;
  never accept on "updated after the write".
- **An unconfirmed write is `deferred`, never `failed`.** amq does not retry a
  failed cohort until new mail arrives, then holds the waker 30–50 s.
- **Record what you typed before typing it** (`store.SetPending`), so a crashed
  injector's leftover is erased before the next doorbell instead of merging.
- **The injector must not depend on the environment.** A waker revived by
  `amq wake repair` runs it without `REX_AMQ_ADAPTER_HOME`; state is derived
  from the root argument.
- **First hook ≠ process start.** Codex and Antigravity run nothing before the
  first turn; lazy adoption plus launching peers with a brief covers it.
- **Label the pane at creation.** A rename after launch races the first hook.
- **Hooks can be clamped and must be approved.** Read the agent's startup
  warnings; install append-only where trust is positional.
- **Some agents always read JSON.** Antigravity needs `{}` even on errors and
  outside Rex (`Render` with an empty `Reply`).
- **Stop may fire on cancel and errors.** Never hold a turn the user cancelled.
- **amq refuses an `--inject-via` under a group- or world-writable directory**
  (`/tmp`, Homebrew's Cellar). Wakers therefore run the wrapper
  `<state>/bin/inject`; put probe injectors under `$TMPDIR`.
- **Fixtures are public.** Scrub emails, account plans, user names, and home
  paths with equal-length replacements before committing.
