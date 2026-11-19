package core

import "fmt"

// Admission (Claim C25): a Rex session is a Hub's or an Epic's, never both.
// A session with a Hub policy or a live personal agent holds only personal
// agents; a personal agent lives only in such a session. Adoption, join, and
// summon all ask AdmitRole, under the lifecycle lock, before anything
// changes. It is the adapter's admission policy, not an OS sandbox.

// Reasons an admission refuses (details.reason of a guard_denied refusal).
const (
	// ReasonRoleLocked: a personal agent never takes another role, and a
	// narrator never takes another role either (a one-way lock).
	ReasonRoleLocked = "personal_role_locked"
	// ReasonSessionPure: a Hub session holds personal agents only; work
	// for other agents opens an Epic.
	ReasonSessionPure = "personal_session_pure"
	// ReasonNoPersonal: an Epic session (its policy, crew, or a live worker)
	// holds no personal agent.
	ReasonNoPersonal = "epic_session_no_personal"
)

// SessionFacts is what admission reads of one Rex session, leaving out the
// agent being admitted.
type SessionFacts struct {
	HubPolicy  bool // the session's policy is a Hub's
	EpicPolicy bool // the session's policy is an Epic's
	Crew       bool // the session works as a crew
	// LivePersonal: a live personal agent is in the session. LiveOther: a
	// live agent of any other role (a narrator included). LiveWorker: a live
	// agent that is not Stealth.
	LivePersonal, LiveOther, LiveWorker bool
}

// Hub reports whether the session is a Hub's: its policy says so, or a
// personal agent lives in it.
func (f SessionFacts) Hub() bool { return f.HubPolicy || f.LivePersonal }

// Epic reports whether the session is an Epic's for a personal agent's
// admission: its policy, its crew, or a live worker says so.
func (f SessionFacts) Epic() bool { return f.EpicPolicy || f.Crew || f.LiveWorker }

// Conflict reports a session that is both: one mixed before admission was
// enforced. It admits nothing new and changes no role; nothing in it is
// killed or changed.
func (f SessionFacts) Conflict() bool { return f.Hub() && (f.EpicPolicy || f.Crew || f.LiveOther) }

// AdmitRole decides whether an agent whose role is from ("" for none, or a
// new agent) may hold role to in a session with facts f: "" admits it,
// otherwise the reason it is refused.
func AdmitRole(f SessionFacts, from, to string) string {
	switch {
	case from == PersonalRole && to != PersonalRole, from == NarratorRole && to != NarratorRole:
		return ReasonRoleLocked
	case to == PersonalRole && f.Epic():
		return ReasonNoPersonal
	case to != PersonalRole && f.Hub():
		return ReasonSessionPure
	}
	return ""
}

// AutoPersonal reports whether an agent adopted with no role becomes the
// session's personal agent: nothing launched it for another purpose (no
// role label, no summon, not unattended), the session is no Epic's (an
// Epic's policy beats its directory: an Epic opened in the Hub stays an
// Epic), and it works in a Hub directory or the session is a Hub's already.
func AutoPersonal(f SessionFacts, labelled, summoned, unattended, inHub bool) bool {
	return !labelled && !summoned && !unattended && !f.EpicPolicy && !f.Crew && !f.LiveOther && (inHub || f.Hub())
}

// HubRefusalNotice is what an agent refused admission to a Hub session is
// told instead of an identity.
const HubRefusalNotice = "AMQ: this Rex session is a Hub, for personal agents only, so this agent got no AMQ identity, mailbox, or lease. Multi-agent work opens an Epic: `rex-amq-adapter epic open <name> --cwd <dir> --asked \"<the user's words>\"`."

// ObligationNotice is the start-notice line for n (> 0) Epics linked to a
// personal agent's Hub whose hand-back waits for it; "" for none.
func ObligationNotice(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 linked Epic has a pending hand-back: `rex-amq-adapter epic list`."
	}
	return fmt.Sprintf("%d linked Epics have pending hand-backs: `rex-amq-adapter epic list`.", n)
}
