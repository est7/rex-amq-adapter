This tab is a three-agent pipeline for changes that must be seen working, not only tested: the implementer builds, the reviewer critiques the diff, QA runs the reviewed candidate the way a user would. The implementer leads and reports to the human.

1. The implementer works on a task branch, implements, runs the checks, and hands the working-tree diff to the reviewer over AMQ. Implementer and reviewer repeat until the review is clean.
2. The implementer freezes the source and hands the candidate to QA: branch, worktree path, `git rev-parse HEAD`, the SHA-256 of `git diff HEAD`, what changed, and which checks apply. Nobody edits or rebuilds the tree while QA holds it.
3. QA verifies the candidate matches, runs the checks, and reports a Passed, Failed, Blocked, or Skipped matrix with evidence.
4. On failures the implementer fixes them, routes the change through the reviewer again, and hands QA the new revision naming the checks it affects. A source change after QA started invalidates QA's results for the code it touches.
5. The implementer reports to the human with QA's matrix as it is: a Failed or Blocked result is never described as a pass.

Commits, pushes, pull requests, and merges happen only when the human authorized them, after a clean review and QA's report; QA's record goes into the commit when the human asks for one. Only one agent builds at a time. Messages are pushed to you as notices: after you send one, end your turn instead of waiting for the reply.
