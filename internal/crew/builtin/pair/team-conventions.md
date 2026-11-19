---
lead: implementer
members: implementer, reviewer
---
This crew is a two-agent loop: the implementer builds, the reviewer critiques. The human gives the brief to the implementer, who leads and reports back.

1. The implementer checks the repository status, works on a Slice or feature branch, implements, and runs the relevant checks.
2. The implementer hands off to the reviewer over AMQ: branch, summary, changed files, checks run. Nothing is committed yet; review reads the working-tree diff.
3. The reviewer replies with must-fix findings first, or "clean review"; the two repeat until clean, the tree untouched during a review. A brief that says to hand the branch back instead (an orchestrator's) wins: whoever briefed the implementer owns the review.
4. The implementer reports to the human: what changed, how it was verified, what review raised and how it was resolved.

An Epic opened as this crew with a goal contract (`rex-amq-adapter epic open <name> --cwd <dir> --crew pair --goal <file>`) has no orchestrator: its implementer leads the contract's Slices, at most 3, one after another. The contract replaces steps 2 to 4 and the working-tree review of the role prompt: the implementer loads skill `run-goal` and runs each Slice on its own branch in the `--lane 1` worktree, a stacked Slice cut `--from <parent_tip>`, committing green Checkpoints as it goes; then it starts a reviewer of another model family (`rex-amq-adapter agent summon reviewer --grant <file> --kind todo --subject … --body …`, naming each branch and its base) and keeps it for every Slice, and fixes each must-fix as a new Checkpoint. It proposes or lands each branch as far as the contract's SCOPE allows, and ends by recording its final report with `rex-amq-adapter epic handback @<file>`, which tells the Epic's narrator.

Commits, pushes, pull requests, and merges happen only when authorized, after a clean review, or as far as the brief authorizes.

Review tiers (`solo-loop` and `council`; the rest is in `rex-amq-adapter skill show review --text`). By default one seat: at most 3 rounds (5 for security, auth, data loss, or concurrency with an open `blocker` or `should`); the budget's end is not approval. On "council": `agent summon reviewer --council --cwd <worktree>`, one seat per harness, each giving `SCORE: <n>/10`, until every seat is satisfied. An item disputed after two replies goes to the human with both sides. After you send a message, end your turn.
