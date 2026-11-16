---
lead: architect
members: architect, architect
---
This crew argues a design to a decision. Two or more agents, usually architects, work the same question; the human decides. The agent the human briefed leads: it runs the rounds, keeps the claim ledger, and writes the report. Members send only to the lead, who relays, so every round starts from the same inputs.

1. Seal. Before forwarding the question, the lead writes its own proposal and holds it unsent, then sends the question to every member.
2. Proposals. Each agent studies the question and the code and sends the lead its proposal: the recommendation, the alternatives it rejected and why, and at most seven numbered claims. A claim is one falsifiable statement (a decision, risk, trade-off, or assumption) with its file:line or executed probe evidence and what would change your mind.
3. Ledger. When every proposal is in, the lead numbers the claims C1, C2, … and broadcasts the ledger with every proposal verbatim. Nobody sees another proposal before this broadcast: until then nobody searches session transcripts (`nmem t`, `rex-history`, transcript files): only the repository and the brief's files.
4. Debate. Each agent answers every active claim with agree, disagree, or revise (with replacement text), each with a one-line rationale that cites evidence, and may add new claims. A changed stance names the argument or evidence behind it. The lead then applies the round: a revision replaces a claim only when its proposer accepts it; otherwise both versions stand as separate claims. Two claims merge only when both proposers agree. The lead broadcasts the updated ledger with the round's answers verbatim.
5. Stop. After at least one debate round, stop when every answer is agree and nothing was revised or added; otherwise stop after the third.
6. Vote. Each agent votes accept or reject on every active claim, with a reason for each reject. A claim is agreed only when every remaining agent accepts it.
7. Report. The lead drafts: status (consensus: every claim agreed; partial: some; unresolved: none; interrupted: fewer than two agents remained); the agreed claims as the design; each split claim with its votes, the crux, and the evidence that would settle it. Each member may return a dissent of at most five lines, which the lead includes verbatim. The lead does not reword claims here. Never present a disagreement as consensus.

When a member is gone (`rex-amq-adapter who`) or idle (`rex block call -b <block> program_status`) without answering the current round, the lead asks once; if it idles again, the lead continues without it and records the drop in the report.

Nobody edits product code; safe, self-cleaning probes outside the repository (removed before reporting) are expected when runtime behavior can be observed, not assumed or deferred to the human. The lead writes a design document only when the human asked for one. Messages arrive as notices: after you send one, end your turn. Wait inside a turn only for something bounded a check needs (CI).
