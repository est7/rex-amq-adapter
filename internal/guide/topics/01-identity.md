---
topic: identity
summary: Your AMQ handle and role, how to load your identity, and what to do without one
---
# Identity

Every Claude Code, Codex, Antigravity (`agy`), or Pi session started in a Rex
terminal is reachable over AMQ under a handle of its kind and a number:
`codex-01`. Its role comes from the label a peer gave the pane (or from
`rex-amq-adapter agent join`) and is kept apart from the handle; the adapter
relabels the pane `<role>(<handle>)`, such as `reviewer(codex-01)`, or just the
handle without a role. Mail the handle, never the label. A resumed session gets
its handle and role back, in any pane.

The adapter owns one AMQ root per user, provisions your mailbox when your
session starts, and tells you when mail arrives. Use
`rex-amq-adapter agent list` to discover agents in your Rex session and the
adapter's mail commands (`rex-amq-adapter mail send`, `mail reply`,
`mail drain`) to exchange mail: they find your identity themselves and check,
before amq runs, that the mail may go where you address it. `amq coop`,
`--project` / `--session` routing, and AMQ skills written for other
environments do not apply here: in a Rex terminal ignore `herdr-amq-adapter`,
`orch`, and `orch-worker`, and their identity files and commands.

## Load your identity

Each shell needs it; tool calls run in fresh shells:

```bash
eval "$(rex-amq-adapter agent env --sh)" && rex-amq-adapter agent list
```

`rex-amq-adapter agent env` reports `AM_ROOT`, `AM_ME`, your role, and your
pane; with `--sh` it prints `export` lines (and `false` on failure, so the
`&&` stops). An Epic's agent, which runs unattended, also gets
`ME_UNATTENDED=1`. The mail commands need no `agent env` first.

Your identity is your own session's, found from the session id your harness
sets in each tool shell, so when the inherited `REX_BLOCK` names another pane,
or none, `agent env` also sets `REX_BLOCK` and `REX_SESSION` to yours.

A session no hook adopted (a `claude -p` that another agent runs, an
Antigravity subagent) has no identity, even in an adopted agent's pane. To run
a headless harness from your tool shell (`codex exec`, `claude -p`, `agy -p`,
`pi -p`, a probe of a model or a flag), start it as
`env -u REX_BLOCK -u REX_SESSION <cli> ...`, so its hooks see no Rex pane at
all. The adapter also ignores every hook of a harness started under your
pane's live agent, so it cannot take your identity, park you as replaced, or
read your mail.

Notices from the adapter quote the absolute path of `rex-amq-adapter`; use it
when the bare command is not on `PATH`.

Codex must run with `--no-daemon`: under its shared daemon one Codex process
runs every pane's threads, so the adapter refuses to adopt it rather than hand
one pane's identity to another, and `agent env` reports none.

## No identity

If `agent env` reports no identity (`no_identity`, exit 3), stop AMQ
operations.

- Codex without `--no-daemon`: have the user exit and resume in the same Rex
  pane with `codex --no-daemon resume <session-id>`, using the actual
  conversation id.
- Otherwise check that the adapter's hooks are installed and approved
  (`rex-amq-adapter install status`); restart the agent in Rex if it started
  before installation.

A missing identity does not prevent launching a peer the user asked for; its
reply over AMQ must wait until your identity is available. The exception is a
Claude Code background session that `agent env` says no pane shows: its
`REX_BLOCK` and `REX_SESSION` name another terminal's pane, and `agent env`
unsets them, so ask the user where to place a peer instead.

Loaded with no notice and no request (the user just invoked this skill): load
your identity, drain once (`rex-amq-adapter mail drain --include-body`), run
`rex-amq-adapter agent list`, report what you found, and ask whom to message
about what.

The other topics: `rex-amq-adapter skill list`, then
`rex-amq-adapter skill show <topic> --text`.
