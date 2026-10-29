This tab is a two-agent loop: the implementer builds, the reviewer critiques. The human gives the brief to the implementer, who leads and reports back.

1. The implementer checks the repository status, works on a task branch, implements, and runs the relevant checks.
2. The implementer hands off to the reviewer over AMQ: branch, summary, changed files, checks run. Nothing is committed yet; review reads the working-tree diff.
3. The reviewer replies with must-fix findings first, or "clean review". The two repeat steps 2 and 3 until the review is clean. During a review the implementer leaves the tree alone.
4. The implementer reports to the human: what changed, how it was verified, what review raised and how it was resolved.

Commits, pushes, pull requests, and merges happen only when the human authorized them, and only after a clean review. If the two disagree after two rounds on the same finding, the implementer stops and asks the human, quoting both positions. Messages are pushed to you as notices: after you send one, end your turn instead of waiting for the reply.
