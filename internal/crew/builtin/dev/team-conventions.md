---
lead: architect
members: architect, implementer, reviewer
---
This tab ships a feature end to end: the architect decomposes, the implementer builds, the reviewer audits. The human gives the goal to the architect, who leads and reports back.

1. The architect reads the goal and the code, asks the human one question if a load-bearing detail is ambiguous, then sends the implementer one task at a time over AMQ: what to change, where, and acceptance criteria someone can check.
2. The implementer builds the task on a task branch, runs the checks, and hands the working-tree diff to the reviewer. The two repeat until the review is clean, then the implementer tells the architect the task is done, with the checks and review outcome. When the implementer's brief opens with make-goal's grant line, skill `autopilot` governs instead: this review loop does not run, the implementer hands its task branch and log to whoever briefed it, and that agent owns the review, every must-fix, and the landing.
3. The architect compares the result with the acceptance criteria. A gap becomes a fix task for the implementer; a met task lets the architect send the next one.
4. When every task is done, the architect reports to the human: what shipped, how each criterion was verified, and the follow-ups it deferred.

The architect does not edit code; the reviewer does not edit code. Commits, pushes, pull requests, and merges happen only when the human authorized them, after a clean review; a brief with the grant line authorizes checkpoint commits on the task branch. Messages are pushed to you as notices: after you send one, end your turn instead of waiting for the reply. Wait inside a turn only for something bounded that a check itself needs, such as watching CI.
