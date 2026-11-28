---
topic: crews
summary: Pick a built-in crew, read its briefing, join it, and plan Slices and seats
---
# Plan a crew

Pick the team before starting anyone. The built-in crews, each claimed by one
Rex session, lead first:

| Crew | For | Members |
|---|---|---|
| `pair` | build and review one change until the review is clean | implementer, reviewer |
| `ship` | `pair`, then QA runs the reviewed candidate and reports a verdict | implementer, reviewer, qa |
| `dev` | an architect splits a goal into tasks for an implementer and reviewer | architect, implementer, reviewer |
| `docs` | writers draft, an editor checks them against the code (an architect, when present, partitions and leads) | architect, writer, editor |
| `design` | architects argue a design to a decision through sealed proposals (no session-transcript search before the ledger) and a claim ledger | architect, architect |

```bash
rex-amq-adapter crew list                    # crews claimed here, their leads and members, and the roles of a session of no crew
rex-amq-adapter crew show pair --text        # a crew's briefing and conventions
rex-amq-adapter crew show --role reviewer    # one role's briefing, prompt, and routing; changes nothing
rex-amq-adapter crew show --role personal --text   # the personal agent's briefing, with the crew selection table
rex-amq-adapter agent join pair              # claim pair for this session and take its lead role
rex-amq-adapter agent join --role reviewer   # take one role where you stand
```

`crew list` adds the project's and user's own crews and roles. A crew's
conventions bind every agent in its session, so claim a crew (`agent join
<crew>`, or `epic open --crew`) only when the work follows them. A session
works as one crew: `agent join` refuses a second one while other agents are in
it, so another crew's goal is another Epic.

Which crew an Epic gets is not yours to invent: the personal role prompt owns
that selection table. Read it with `rex-amq-adapter crew show --role personal
--text` instead of guessing here.

When no crew fits (an audit, research, a migration), run the goal with an
`orchestrator` that starts each peer in a tab of its own under a built-in role
(`architect`, `reviewer`, `qa`, `writer`, ...): the role briefs it and your
brief scopes it.

## Size and routing

- **Size.** A summon refuses past 6 live agents in a session; a `narrator` and
  a `personal` agent do not count. Agents you start with `rex split` or
  `rex run` are not checked, so count them yourself and keep one session within
  6, yourself included.
- **Model families.** Each role starts on the harness, model, and effort its
  `routing.toml` names (`rex-amq-adapter crew show --role <role> --text` prints
  them and the file they come from). A summon starts `reviewer`, `editor`, and
  `qa` on the first harness in that list whose model is another family than
  yours (Antigravity running a Claude model is Anthropic), passing over one that
  is not installed or not ready, and every other role on its first harness,
  unless `--harness` says otherwise. When you start peers yourself, give the
  reviewer another family than the implementer.

## Several Slices

- One implementer per Slice, in that Slice's worktree
  (`agent summon implementer --cwd <worktree>`), and one reviewer or QA kept
  open across Slices (a summon without `--cwd` reaches it).
- A `pair` crew's implementer leads up to 3 Slices of a contract, one after
  another.
- **Council seats.** A read-only role (reviewer, editor, qa, architect) reads a
  Slice worktree without writing it, so up to one seat per harness may share it:
  `agent summon reviewer --council --cwd <worktree>` seats them all at once (see
  the `review` topic). A design Epic's lead architect may instead seat only the
  harnesses it wants, one `agent summon architect --harness <h> --cwd <dir>` per
  seat (see the `summon` topic). A second seat on the same harness, and any
  writer role, is refused.
- **Another Epic.** A goal that is a separate Epic gets its own Rex session with
  its own lead, not more panes here. See the `epics` topic.
