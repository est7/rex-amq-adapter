package app

import (
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
)

// agyApprovePlan is the message Antigravity (agy) reports, as a blocked
// permission, for an artifact it wrote under brain/<conversation>/ and
// considers unreviewed. agy reports it at every turn end after such a write,
// whatever RequestFeedback or artifactReviewPolicy say, while its keys only
// go to the review when the full-screen /artifact viewer is open.
const agyApprovePlan = "Approve plan"

// isAgyApprovePlan reports whether st is agy's "Approve plan" status, the only
// blocked status that may be a passive banner rather than a prompt. kind is
// the adopted agent's kind, "" before adoption; agy also names itself in the
// status record.
func isAgyApprovePlan(kind core.Kind, st rex.Status) bool {
	agy := kind == core.Agy || st.App == "antigravity" || st.EffectiveApp == "antigravity"
	return agy && core.AgentState(st.State) == core.StateBlocked && st.Kind == "permission" && st.Msg == agyApprovePlan
}

// normalizeProgramStatus is st as the agent's readiness: agy's "Approve plan"
// with its ruled > box on screen and empty, and no trust dialog, is idle; the
// banner holds nothing, and the box takes the next prompt. Every other status,
// that one with the /artifact viewer open (no box), a draft, or a trust
// screen included, is returned unchanged. vt is the block's captured screen.
func normalizeProgramStatus(kind core.Kind, st rex.Status, vt string) rex.Status {
	if !isAgyApprovePlan(kind, st) {
		return st
	}
	if _, trust := screen.TrustScreen(string(core.Agy), vt); trust || screen.Check(string(core.Agy), vt) != screen.Empty {
		return st
	}
	st.State, st.Kind, st.Msg = string(core.StateIdle), "", ""
	return st
}
