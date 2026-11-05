package app

import (
	"path/filepath"
	"strings"
)

// agySubagent reports whether conversation is a subagent another
// Antigravity conversation started. agy runs a subagent inside the parent's
// process, with SessionStart, PreInvocation, and Stop hooks of its own under
// the subagent's conversationId, and strips the parent's id from the hook
// input; it writes brain/<parent>/.system_generated/subagents/<id>.json
// before the subagent's first hook (agy 1.5.1; docs/design.md section 4).
// Workaround for that missing field: remove once agy's hook input names the
// parent conversation.
func (e Env) agySubagent(conversation string) bool {
	if e.AgyBrainDir == "" || conversation == "" || strings.ContainsAny(conversation, `/\*?[`) {
		return false
	}
	found, _ := filepath.Glob(filepath.Join(e.AgyBrainDir, "*", ".system_generated", "subagents", conversation+".json"))
	return len(found) > 0
}
