package core

// The gateway policy decides which agents may exchange mail on the one AMQ
// root every agent shares. Rex sessions are Epics: an Epic's members work
// together and nothing crosses between Epics, except one channel the user
// opens with link: a Personal agent (its own Rex session) and the Narrator of
// a linked Epic. The Narrator observes its Epic without being mailed; its one
// write into the Epic is a user-control directive to the Orchestrator.
//
// The policy holds for every supported path (the adapter's managed mail
// commands and its arrival gate). Any process of the same UNIX user can still
// write the mailbox files directly: the gateway is not a sandbox.

// Roles the gateway treats specially. Every other role (none, orchestrator,
// implementer, reviewer, ...) is an ordinary member of its Epic.
const (
	PersonalRole = "personal"
	NarratorRole = "narrator"
)

// SessionKey names a Rex session (an Epic, or a Personal agent's session)
// across servers.
type SessionKey struct {
	Server    string `json:"server"`
	SessionID string `json:"session_id"`
}

// Endpoint is an agent as the gateway sees it at one moment: its handle and
// role, and where it is. Generation counts the moves of the handle between
// sessions (store.Record.Generation), so mail addressed to it before a move
// is told from mail addressed to it now.
type Endpoint struct {
	Handle     Handle `json:"handle"`
	Role       string `json:"role,omitempty"`
	Server     string `json:"server,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Generation int    `json:"generation,omitempty"`
}

// Session is the session e is in.
func (e Endpoint) Session() SessionKey { return SessionKey{Server: e.Server, SessionID: e.SessionID} }

// placed reports whether e is known to be in a Rex session.
func (e Endpoint) placed() bool { return e.Server != "" && e.SessionID != "" }

// Link is a grant the user made: Personal's session may exchange mail with
// the Narrator of Epic.
type Link struct {
	Personal SessionKey `json:"personal"`
	Epic     SessionKey `json:"epic"`
}

// OpKind is what a message was sent as.
type OpKind string

const (
	OpSend  OpKind = "send"
	OpReply OpKind = "reply"
	// OpControl relays a user-control directive (a change of requirement, a
	// start, a decision) to an Epic's Orchestrator, under ControlRequestID.
	OpControl OpKind = "control"
)

// Operation is a message as it was sent. System is true only when the
// adapter's own provenance record says the adapter sent it; a header label or
// a "system" sender name never sets it. Addressed, for a message already
// sent, is the recipient as it was when the message was sent: a reader that
// has moved since is no longer it.
type Operation struct {
	Kind             OpKind    `json:"op"`
	ControlRequestID string    `json:"control_request_id,omitempty"`
	System           bool      `json:"system,omitempty"`
	Addressed        *Endpoint `json:"-"`
}

// Decision is the gateway's answer; Reason names the rule that decided.
type Decision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason"`
}

// Reasons a decision gives.
const (
	ReasonSelf             = "self"
	ReasonSameEpic         = "same_epic"
	ReasonLinked           = "personal_narrator_linked"
	ReasonSystem           = "adapter_system_mail"
	ReasonControl          = "narrator_control"
	ReasonFenced           = "addressed_to_prior_placement"
	ReasonNotLinked        = "personal_narrator_not_linked"
	ReasonPersonalOnly     = "personal_reaches_only_a_linked_narrator"
	ReasonUnplaced         = "endpoint_not_in_a_session"
	ReasonCrossEpic        = "cross_epic"
	ReasonNarratorObserves = "narrator_takes_no_member_mail"
	ReasonNarratorRoutine  = "narrator_sends_orchestrator_control_only"
	ReasonNarratorWorker   = "narrator_never_mails_workers"
)

func allow(reason string) Decision { return Decision{Allow: true, Reason: reason} }
func deny(reason string) Decision  { return Decision{Reason: reason} }

// CanExchange decides whether sender may send op to recipient, or, for a
// message already sent (op.Addressed set), whether recipient may see it now.
// For a sent message, sender is the sender as it was at sending.
func CanExchange(sender, recipient Endpoint, op Operation, links []Link) Decision {
	if a := op.Addressed; a != nil && (a.Server != recipient.Server || a.SessionID != recipient.SessionID || a.Generation != recipient.Generation) {
		return deny(ReasonFenced)
	}
	if sender.Handle == recipient.Handle {
		return allow(ReasonSelf)
	}
	if sender.Role == PersonalRole || recipient.Role == PersonalRole {
		return personal(sender, recipient, links)
	}
	if !sender.placed() || !recipient.placed() {
		return deny(ReasonUnplaced)
	}
	if sender.Session() != recipient.Session() {
		return deny(ReasonCrossEpic)
	}
	switch {
	case recipient.Role == NarratorRole:
		if op.System && sender.Role != NarratorRole {
			return allow(ReasonSystem)
		}
		return deny(ReasonNarratorObserves)
	case sender.Role == NarratorRole && recipient.Role == OrchestratorRole:
		if op.Kind == OpControl && op.ControlRequestID != "" {
			return allow(ReasonControl)
		}
		return deny(ReasonNarratorRoutine)
	case sender.Role == NarratorRole:
		return deny(ReasonNarratorWorker)
	}
	return allow(ReasonSameEpic)
}

// personal decides an exchange with a Personal agent on either side: only a
// Narrator of an Epic linked to the Personal agent's session qualifies.
func personal(sender, recipient Endpoint, links []Link) Decision {
	p, other := sender, recipient
	if recipient.Role == PersonalRole {
		p, other = recipient, sender
	}
	if other.Role != NarratorRole {
		return deny(ReasonPersonalOnly)
	}
	if !p.placed() || !other.placed() {
		return deny(ReasonUnplaced)
	}
	for _, l := range links {
		if l.Personal == p.Session() && l.Epic == other.Session() {
			return allow(ReasonLinked)
		}
	}
	return deny(ReasonNotLinked)
}
