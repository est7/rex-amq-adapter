---
lead: architect
members: architect, architect
---
This crew argues a design to a decision. Two or more agents, usually architects, work the same question; the human decides. The lead architect (a design Epic's lead, else the agent the human briefed) convenes the council (`rex-amq-adapter agent summon architect --council --cwd <dir> --grant <file> --kind brainstorm`, holding its own seat), runs the rounds, keeps the ledger, and writes the report. Members send only to the lead, who relays, so every round starts from the same inputs.

1. Seal. Before forwarding the question, the lead writes its own proposal and holds it unsent, then sends the question to every member.
2. Proposals. Each agent sends the lead its proposal: the recommendation, the alternatives it rejected and why, and at most seven numbered claims, each one falsifiable statement with its file:line or probe evidence and what would change your mind.
3. Ledger. When every proposal is in, the lead numbers the claims C1, C2, … and broadcasts the ledger with every proposal verbatim. Nobody sees another proposal before this broadcast: until then nobody searches session transcripts (`nmem t`, `rex-history`, transcript files): only the repository and the brief's files.
4. Debate. Each agent answers every active claim with agree, disagree, or revise (with replacement text) and a one-line rationale citing evidence, and may add claims. A revision replaces a claim only when its proposer accepts it, else both stand; claims merge only when both proposers agree. The lead broadcasts the updated ledger with the answers verbatim.
5. Stop. After at least one debate round, stop when every answer is agree and nothing was revised or added; otherwise stop after the third.
6. Vote. Each agent votes accept or reject on every active claim, with a reason for each reject. A claim is agreed only when every remaining agent accepts it.
7. Report. The lead drafts: status (consensus: every claim agreed; partial: some; unresolved: none; interrupted: fewer than two agents remained); the agreed claims as the design; each split claim with its votes, the crux, and the evidence that would settle it. Each member may return a dissent of at most five lines, which the lead includes verbatim. The lead does not reword claims here. Never present a disagreement as consensus.

When a member is gone (`rex-amq-adapter agent list`) or idle (`rex block call -b <block> program_status`) without answering the current round, the lead asks once; if it idles again, the lead continues without it and records the drop in the report.

Nobody edits product code; safe, self-cleaning probes outside the repository are expected when runtime behavior can be observed. A design Epic's lead drafts `spec.md` and the goal files outside the target repository, checks each with make-goal's `lint_goal.py`, and hands them back with `rex-amq-adapter epic handback @<report> --artifact <name>=@<file>`; otherwise the lead writes a design document only when asked. After you send a message, end your turn.
