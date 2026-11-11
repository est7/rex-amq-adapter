---
lead: implementer
members: implementer, reviewer, qa
---
This tab is a three-agent pipeline for changes that must be seen working, not only tested: the implementer builds, the reviewer critiques the diff, QA runs the reviewed candidate the way a user would. The implementer leads and reports to the human.

1. The implementer works on a Slice or feature branch, implements, runs the checks, and hands the working-tree diff, untracked files included, to the reviewer over AMQ. Implementer and reviewer repeat until the review is clean. When the implementer's brief says to skip this loop and hand the branch back (an orchestrator running a goal does), the implementer does that instead, and whoever briefed it owns the review and every must-fix. The orchestrator then hands the Slice branch to QA (step 2) and QA reports to it.
2. The implementer freezes the source and hands the candidate to QA: branch, worktree path, `git rev-parse HEAD`, the SHA-256 of `git diff HEAD` plus the untracked files (`git ls-files --others --exclude-standard`, each with its content), what changed, and which checks apply. Nobody edits or rebuilds the tree while QA holds it.
3. QA verifies the candidate matches, runs the checks, and reports a Passed, Failed, Blocked, or Skipped matrix with evidence.
4. On failures the implementer fixes them, routes the change through the reviewer again, and hands QA the new revision naming the checks it affects; when an orchestrator handed QA the candidate, it runs this loop. A source change after QA started invalidates QA's results for the code it touches.
5. The implementer reports to the human with QA's matrix as it is: a Failed or Blocked result is never described as a pass.

Commits, pushes, pull requests, and merges happen only when the human authorized them, after a clean review and QA's report, or as far as the brief they were given authorizes; when the human asks for QA's record in the commit, the commit message carries QA's one-line verdict and the path of its report, which stays outside the repository. If implementer and reviewer disagree after two rounds on the same finding, the implementer stops and asks the human, or the orchestrator that briefed it, quoting both positions. Only one agent builds at a time. Messages are pushed to you as notices: after you send one, end your turn instead of waiting for the reply. Wait inside a turn only for something bounded that a check itself needs, such as watching CI.
