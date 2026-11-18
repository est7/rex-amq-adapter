---
topic: task-label
summary: Show your current task in your pane label; never rename the Rex session
---
# Show your task in your pane

Once you know what you are working on, usually from the user's first request,
put it after your role and handle in your pane's label, in a few words, and run
it again when the task changes. Do it whether or not you use AMQ:

```bash
rex-amq-adapter agent title "fix login redirect"   # pane: reviewer(claude-01) | fix login redirect
rex-amq-adapter agent title                        # clear it
```

The adapter composes the label, `<role>(<handle>) | <task>`; it cuts a long
task to 32 columns. A peer you start gets its task from its launch label (see
the `peers` topic), so it shows one from its first turn. The task follows your
handle, so a resume in another pane shows it again.

Do not rename your pane with `rex block rename`: the next adoption would write
the adapter's label back. To take a role yourself, run
`rex-amq-adapter agent join --role <role>` (or `agent join <crew>`, see the
`crews` topic); it relabels the pane and prints your briefing. A tab holding
one agent shows that agent's pane label too, and follows it.

The Rex session's label is not yours: one Rex session is one Epic, and its
label names the human's Epic. Never run `rex session rename` for your own task,
even when Rex's own guidance suggests it; your task goes in your pane label.
When the session names an Epic, Claude Code and Pi also show it in your
conversation title, `<role>(<handle>) | [<epic>] <task>`.
