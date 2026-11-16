---
lead: implementer
members: implementer, reviewer
---
This crew is a two-agent loop: the implementer builds, the reviewer critiques. The human gives the brief to the implementer, who leads and reports back.

1. The implementer checks the repository status, works on a Slice or feature branch, implements, and runs the relevant checks.
2. The implementer hands off to the reviewer over AMQ: branch, summary, changed files, checks run. Nothing is committed yet; review reads the working-tree diff.
3. The reviewer replies with must-fix findings first, or "clean review". The two repeat steps 2 and 3 until the review is clean. During a review the implementer leaves the tree alone. When the implementer's brief says to skip this loop and hand the branch back (an orchestrator running a goal does), the implementer does that instead, and whoever briefed it owns the review and every must-fix.
4. The implementer reports to the human: what changed, how it was verified, what review raised and how it was resolved.

An Epic opened as this crew with a goal contract (`rex-amq-adapter epic <name> --crew pair --goal <file>`) has no orchestrator: its implementer leads one Slice and reports to no one in the session. The contract replaces steps 2 to 4 and the working-tree review of the role prompt: the implementer loads skill `run-goal` and runs the contract on a Slice branch, committing green Checkpoints as it goes; then it starts a reviewer of another model family (`rex-amq-adapter summon reviewer --grant <file> --kind todo --subject … --body …`, naming the branch and its base) and fixes each must-fix as a new Checkpoint. It proposes or lands the branch as far as the contract's SCOPE allows, and ends by recording its final report with `rex-amq-adapter hand-back @<file>`, which tells the Epic's narrator.

Commits, pushes, pull requests, and merges happen only when the human authorized them, and only after a clean review, or as far as the brief they were given authorizes.

Review tiers (tstack's `sol-loop` and `council`; the rest is in skill `rex-amq-adapter`, Review tiers). By default one seat: at most 3 rounds, 5 only for security, permissions, auth, data loss, or state/concurrency while a `blocker` or `should` stays open with evidence; stop when only nits remain; the budget's end is not approval. When the human says "council" (a score after it is the bar): fix known issues first, then `summon reviewer --council --cwd <worktree>`, one seat per harness, each giving `SCORE: <n>/10`; report a missing seat, never substitute; edit nothing while a seat reads; rounds until every seat is satisfied; scores go in a `## Council review` table. In both, fight scope creep; an item disputed after two replies goes to the human with both sides. Messages are pushed to you as notices: after you send one, end your turn instead of waiting for the reply. Wait inside a turn only for something bounded that a check itself needs, such as watching CI.
