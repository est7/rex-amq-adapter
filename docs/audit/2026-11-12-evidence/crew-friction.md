# Crew/goal path friction observed during this run

## F1: skill guide too thin to start a goal crew without reading source (reported by est9, 2026-11-12)

Observed: after `/rex-amq-adapter` loaded (guide from `rex-amq-adapter __internal skill`), the lead
ran three exploratory Bash calls just to learn what crews/roles do before it could plan a crew:
1. `rex-amq-adapter crews --help` -> no help; prints the same table again (subcommand ignores --help)
2. `ls -R internal/crew/builtin` + grep for "conventions" -> searched source for crew definitions
3. `cat design/team-conventions.md roles/orchestrator.md roles/qa.md` -> read role prompts from source
Later two more: `cat roles/reviewer.md roles/architect.md` (advisor flagged that role prompts
reach a peer before the brief, so a role must be read before choosing it).

Root cause (located):
- `crews` lists names, lead and roles but no purpose line per crew or per role; the guide only says
  "implementer, reviewer, qa, architect, writer, editor, orchestrator are built in".
- The guide has no recipe for "the user asks for a crew to run a goal" when no built-in crew fits
  (audit, research): whether to `join --role orchestrator` in an un-crewed tab, which roles fit
  read-only work, and that `reviewer` and `qa` prompts say "stay idle until handed a candidate"
  (so the brief must count as the handoff).
- Role prompts live only in the binary's embedded files; there is no `rex-amq-adapter crews show <crew|role>`.
- `crews --help` is silently treated as `crews`.
- The workflow is installed only as a binary: an agent outside this repo cannot read internal/crew/builtin at all.

Fix sketch: (a) `crews` prints a one-line purpose per crew and role (from front matter);
(b) `crews show <name>` prints the conventions/role prompt; (c) guide adds a 5-line
"crew for a goal" recipe (orchestrator lead + roles, read-only audits use reviewer/architect/qa with
the brief as handoff); (d) subcommands honor --help.

## F2: lead drafted `SCOPE: none (local commits only)` although no scope `local` exists (reported by est9)

No surviving text grants a local scope (grep over make-goal, run-goal, decision-log finds only the
lint's refusal and its fail fixture; removed in ~/.agents 8c64b85). The value came from the lead's
own inference, steered by wording that leaves room for it:
- make-goal/SKILL.md:53 "`push+PR` unless the user said otherwise": implies a third value; the lead
  read "测试一下" as "otherwise". The only alternative is `merge`, so the sentence should say there
  are exactly two values and a local/none scope fails lint.
- same line, "A read-only goal says so under BOUNDARIES": the lead merged "read-only on code" with
  "no push" instead of keeping them separate.
- template SCOPE bullets "each outward action the run may take, by name" read as if outward actions
  are opt-in per contract.
- ~/.claude/CLAUDE.md:84 "Commit or push only when est9 ... asks" and the Outward-action door
  primed caution; CLAUDE.md:39 does say a goal contract always names pushing its branches, but
  make-goal does not repeat it where SCOPE is written.
Real gap behind the instinct: est7/rex-amq-adapter is PUBLIC, so pushing a security-audit branch
publishes unfixed findings — not a two-way door. make-goal has no sanctioned way to express
"hold the push" for such a run; the lead had to hold it ad hoc under Blocked on me.
Fix sketch: SKILL.md:53 -> "exactly push+PR or merge; there is no local scope (lint fails it). When
pushing would publish something sensitive (public repo + security findings), keep push+PR and add a
BOUNDARIES line holding the push; run-goal treats it as a held outward action."

## F3: multi-agent tab keeps the lead's pane label as its name (by design; UX question, Low)
Observed: `who` TAB = "orchestrator(claude-03) | crew audit goal" for all four agents.
Cause (located + executed): the tab is a `auto` follower (docs/design.md:305, 314-316; internal/core/tab.go:72-91,
internal/app/tab.go:77-104): while it held one live agent it mirrored that agent's pane label; once peers
were split in it froze at the last value. Probe in a disposable session: Rex itself never derives the
window label from a block rename or an OSC title (stays `Window xxxxxxxx`), so the adapter wrote it.
Not a bug, but the frozen label names one agent and its handle for a tab of four, so `who`'s TAB column
reads as if every peer were claude-03's pane. Option: when a follower gains a second live agent, drop to
the lead's task alone ("crew audit goal") or keep the placeholder; or have `who` show the tab id there.

## F4: `await --timeout` takes a Go duration (`60s`), the guide's wording invites `60`
Guide: "waits up to 30 s; --timeout changes it". `await <block> --timeout 60` exits 2 "parse error";
the lead had to retry with `60s`. Fix: accept bare seconds, or show `--timeout 60s` in the guide.
