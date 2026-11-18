---
topic: summon
summary: Reach a peer by handle or role; what a summon starts, and its guards
---
# Summon on send

To reach a peer that may not be running, address it by handle or role and let
the adapter deliver, resume, or start it:

```bash
rex-amq-adapter agent summon <handle|role> --subject S --body B|@F|- --kind K [--asked W | --grant F] [--harness H | --council] [--cwd D] [--split] [--timeout D] [--dry-run]
```

```bash
rex-amq-adapter agent summon reviewer --subject "review #42" --body @request.md --kind review_request --asked "让 codex 审一下"
rex-amq-adapter agent summon claude-02 --subject "question" --body "Which base?" --dry-run
```

A live agent of that handle or role in your session gets the mail; when several
agents of a role are live, a summon refuses: name one by handle. A departed one
of this session is resumed in a tab of its own (`--split`: beside you, when the
user asked; never a narrator or personal agent), in the directory it ran in, and
its mail waits for it.

- `--harness` with no `--cwd`: only an agent of the role on that harness counts;
  one on another harness, live or departed, is passed over and gets no mail.
- `--cwd <dir>`: it only ever starts a new agent there, a second one of a live
  role included. It refuses a handle, and a directory a live agent of the role
  already works in (a read-only role only on the same harness: see the `crews`
  topic), and a departed one is not resumed.
- `--council`: a read-only role fans out to one seat per harness of its routing
  (see the `review` topic).

## What it starts

With no live or departed agent to reach, a new agent is started for the role on
its routing (`--harness` picks the CLI), with the message as its brief, in your
directory or the one `--cwd` names, and told once it is adopted. `reviewer`,
`editor`, and `qa` default to the first harness of their routing whose model is
another family than yours, installed, and passing the preflight; every other
role, a custom one included, to the first harness of its routing. The JSON names
the `harness` started and each one `passed_over`, and why.

A launched agent carries a token in its environment that binds its first hook to
this summon. It holds its place in the cap until it is adopted, or two minutes
pass. A resumed agent gets a fixed first prompt that makes it drain its mailbox,
where your message waits.

The command exits 0 only when the mail is delivered or the launched agent was
adopted; a trust screen, blocked dialog, or timeout exits 7 (`not_adopted`)
with the startup state in `data`.

## The guards

The guards decide, and a refusal says why; never retry a refused summon with
other words:

- a personal agent never summons, and neither does any agent of a Hub session
  (exit 3; the refusal names `rex-amq-adapter epic open`): it opens an Epic,
  and the refusal mails and launches nothing;
- only a request summons (`--kind question`, `review_request`, `todo`,
  `brainstorm`); an answer, review_response, status, or decision delivers only;
- no cycle: a handle or role already in your summon chain (the agents that
  summoned you, and you) is refused;
- depth 2 at most: the user's agents are depth 0, what they summon 1;
- 6 live agents per session at most (`cap_reached`, exit 5);
- a new agent starts only with the user's own words in this session asking for
  it (`--asked "<their words>"`, logged as your attestation) or a brief that
  cites their grant file (`--grant <file>`: it must exist, you checked it, and
  the brief names it);
- an agent whose own summoner is no longer recorded may deliver, not start or
  resume: its chain cannot be checked;
- a design Epic's lead architect may seat its own `--council`, which is the one
  read-only fan-out inside such a session.

A refusal exits 3 (`permission_denied`), or 5 for `cap_reached`, and says why.

## Summon log

Every summon is logged (`logs/summon.log`: who, why, parent, depth); list yours
in your hand-back under Changed, one line each from the command's output.
