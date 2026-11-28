---
topic: peers
summary: Find your session-mates, start a peer, confirm it, and retire it when done
---
# Peers

## Session-mates

```bash
rex-amq-adapter agent list                  # agents in this Rex session, including you
rex-amq-adapter agent list --all            # also departed agents reserved for a resume, the narrator, personal agents (a personal agent's peers)
rex-amq-adapter agent list --fields handle,kind,role,tab,agent_session_id,cwd,lifecycle,delivery
```

The Rex session is the awareness boundary: a session holds one task (an Epic)
and at most one crew, and each agent in it has a tab of its own unless the
user asked for a split. Agents in other blocks of your session, in any of its
tabs, are your peers; `agent list` shows each one's tab. Agents in another
session are not discovered or announced. If it lists only you, report that
this session has no peers. Never use `amq who` for discovery: its shared
mailbox directory spans sessions. If the adapter cannot resolve your session,
report the error instead of using a global listing.

Your start notice names the agents already there ("Agents beside you in this
Rex session: ..."), and a notice with your next prompt names each that joins
or leaves. Stop waiting for a reply from an agent that left. In a session with
an orchestrator, it hears of a departure by mail at once. A peer retired as
finished is not such a departure: nobody is mailed. When the user says "the
one beside you", "旁边那个", or "隔壁", they mean the agents of your session.
Reach them with `rex-amq-adapter mail send`, not with your harness's own agent
messaging, which does not know AMQ handles.

Send only to a handle `agent list` shows. A departed agent (`agent list --all`)
keeps its handle for 7 days so its session can be resumed, and mail to it
waits unread until then; start a peer instead unless the user is resuming it.
If you do mail one, a notice names who now runs in that pane and who is in
your session: resend to the agent you meant.

## Start a peer

When the user asks for another agent ("开个 codex 审一下", "spin up a
reviewer"), start it, hand it the brief, and talk to it like any peer. To plan
a team first, read the `crews` topic; for the guards a summon checks, read the
`summon` topic.

0. **Show your own task** first if your pane shows only your handle
   (`rex-amq-adapter agent title "<your task>"`).
1. **Pick a role and name the peer's task.** The pane label is
   `"<role> | <task>"`, such as `"reviewer | fix login redirect"`. The adapter
   names the peer `<kind>-<NN>`, records its role, and relabels the pane
   `reviewer(codex-02) | <task>`. A role with a prompt briefs the peer when it
   starts (`implementer`, `reviewer`, `qa`, `architect`, `writer`, `editor`,
   `orchestrator` are built in; `rex-amq-adapter crew list` shows the rest), so
   keep your brief to the task. Use `orchestrator` when one agent should own a
   goal and coordinate the others.
2. **Write the brief** as the peer's first prompt: the task, the files or diff,
   and that it should report back with `rex-amq-adapter mail send --to <your
   handle>`. An agent is adopted when its first turn starts, and Codex does not
   start a turn on its own, so a peer launched without a prompt never gets a
   mailbox. An independent task in another session reports to its own user; do
   not pass it the handles from your session.
3. **Place it.** Run a `rex` command in the same shell right after
   `eval "$(rex-amq-adapter agent env --sh)"`, so `REX_SESSION` and `REX_BLOCK`
   name your own pane:

   | The peer works on | Command |
   |---|---|
   | the same task, or your crew | `rex-amq-adapter agent summon <role> --subject "<task>" --body "<brief>" --kind todo --asked "<the user's words>"` (a tab of its own; it awaits adoption), or `rex run -s "$REX_SESSION" --label "<label>" -c "$PWD" --focus=false --json -- <launch>` |
   | the same, beside you when the user asked ("split 一下") | `agent summon ... --split`, or `rex split -s "$REX_SESSION" -b "$REX_BLOCK" --split=right --focus=false --label "<label>" -c "$PWD" --json -- <launch>` |
   | a team the user asked for by crew | `rex-amq-adapter agent join <crew>` (you lead), then one tab per member as above |
   | another task or project | `rex new <name> -c <dir> --json`, then `rex run -s <its .session_id> --label "<label>" -c <dir> --focus=false --json -- <launch>` |

   `<launch>` takes the brief as its first prompt: `codex --no-daemon
   "<brief>"`, `claude "<brief>"`, `pi "<brief>"`, or
   `agy --dangerously-skip-permissions --mode auto -i "<brief>"` (without the
   skip flag agy stops before every `amq` command; without `--mode auto` it may
   stop in plan mode). Pass `--no-daemon` for Codex explicitly. Never take
   focus. Label the pane when you create it, never by hand afterwards. A split
   into a pane under 10 rows (`rex block call -b "$REX_BLOCK" size`) cannot draw
   the peer's input box: open a tab instead. When unsure, prefer a tab, and
   tell the user where the peer is.

   Folder trust is the user's to give. `rex-amq-adapter trust check <dir>`
   shows each harness's verdict; for an agy peer you start by hand in a linked
   worktree, pass `--settings "$(rex-amq-adapter trust check <dir> --fields
   settings --json | jq -r .data.settings)"`. When a pane would stop on a trust
   dialog, tell the user to run `rex-amq-adapter trust grant <dir>`; never
   answer the dialog for them. `agent summon` and `epic open` check trust before
   any pane opens: `untrusted_folder` (or, for Codex, `unapproved_hooks`, which
   needs the user's `/hooks` approval) started nothing.
4. **Confirm adoption** before more mail, with the block id you saved:

   ```bash
   rex-amq-adapter agent await <block>   # default 30s; --timeout 90s changes it
   ```

   It exits 0 only for `adopted`, which also gives the `handle`; anything else
   exits 7 (`not_adopted`) with the startup in `data`. `adopted` means the
   mailbox and its waker are ready, not that the peer read your brief.
   `trust_screen` and `blocked` need the user in that pane; `exited` means the
   pane is gone; `timeout` means no adoption finished (a Codex peer whose hooks
   were never approved stays there).

## Finish

`rex-amq-adapter agent retire <handle>` closes a finished peer you started (or
one the user names): it departs the agent, retires its waker, and closes its
pane. It refuses a busy peer (exit 5) unless the user says `--force`, and never
retires a narrator, personal, or orchestrator agent, or you.
`rex-amq-adapter agent collect` reconciles records with Rex and brings back
dead wakers; `--dry-run` shows what it would do.
