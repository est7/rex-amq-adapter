package core

import (
	"fmt"
	"strings"
)

// Peer is a live agent as the window policy sees it.
type Peer struct {
	Handle Handle
	Kind   Kind
	Block  string
}

// WindowMates are the peers whose blocks are in window, other than any in
// self's own block (an agent this one replaced there). windowOf maps a block
// to its window; a peer whose block it does not list is elsewhere. The user
// places agents on one task side by side in a window, so these are the agents
// "beside" this one.
func WindowMates(selfBlock, window string, windowOf map[string]string, peers []Peer) []Peer {
	if window == "" {
		return nil
	}
	var out []Peer
	for _, p := range peers {
		if p.Block != selfBlock && windowOf[p.Block] == window {
			out = append(out, p)
		}
	}
	return out
}

// MatesNotice names an agent's window-mates; "" when it has none.
func MatesNotice(mates []Peer) string {
	if len(mates) == 0 {
		return ""
	}
	names := make([]string, len(mates))
	for i, m := range mates {
		names[i] = fmt.Sprintf("%s (%s)", m.Handle, m.Kind)
	}
	return "Agents beside you in this Rex tab: " + strings.Join(names, ", ") +
		". Reach them with `amq send --to <handle>`."
}

// JoinedNotice tells a window-mate that p started beside it.
func JoinedNotice(p Peer) string {
	return fmt.Sprintf("AMQ: %s (%s) joined your Rex tab. Reach it with `amq send --to %s`.", p.Handle, p.Kind, p.Handle)
}

// LeftNotice tells a window-mate that p's session ended. Its handle stays
// reserved for a resume, so mail still reaches it then.
func LeftNotice(p Peer) string {
	return fmt.Sprintf("AMQ: %s (%s) left your Rex tab. Mail to it waits until its session resumes; do not wait for its reply.", p.Handle, p.Kind)
}

// UndeliveredNotice tells a sender that its mail went to to, an agent that had
// left. occupant is the agent now in to's pane, if any; mates are the sender's
// window-mates, the agents it may have meant.
func UndeliveredNotice(to Handle, occupant *Peer, mates []Peer) string {
	notice := fmt.Sprintf("AMQ: your mail to %s will not be read unless its session resumes: that agent left.", to)
	if occupant != nil {
		notice += fmt.Sprintf(" %s (%s) now runs in its pane.", occupant.Handle, occupant.Kind)
	}
	if beside := MatesNotice(mates); beside != "" {
		return notice + " " + beside
	}
	return notice + " Run `rex-amq-adapter who` for the agents in your Rex tab."
}

// GoalRole is the pane label of the agent that owns a goal. A window holding
// one runs unattended.
const GoalRole = "goal"

// IsGoalAgent reports whether h was named for a pane labelled GoalRole.
func IsGoalAgent(h Handle) bool { return Role(string(h)) == GoalRole }

// GoalPeerLeft is the mail telling a goal agent that p departed. session is
// p's agent session, "" when unknown.
func GoalPeerLeft(p Peer, session string) (subject, body string) {
	subject = fmt.Sprintf("%s left your Rex tab", p.Handle)
	body = fmt.Sprintf("%s (%s) left your Rex tab: it quit, crashed, or its pane closed. You own the goal, so decide what happens next: resume it, start a new agent, or change the plan.", p.Handle, p.Kind)
	if session != "" {
		body += fmt.Sprintf(" To keep its context, resume session %s in a pane; it takes its handle back.", session)
	}
	return subject, body + " It cannot read a reply to this mail."
}

// GoalAgentLeft is the mail telling a surviving agent that the goal agent
// departed.
func GoalAgentLeft(p Peer) (subject, body string) {
	return fmt.Sprintf("%s left your Rex tab", p.Handle), fmt.Sprintf("%s, which owned this tab's goal, left your Rex tab: it quit, crashed, or its pane closed, and nobody is running the goal. Finish the step you are on, say in your final message that the goal stopped and where the work stands, then end your turn: do not take over the goal. It cannot read a reply to this mail.", p.Handle)
}

// DepartureMail says who hears at once that left departed: every goal agent
// among others, the other live agents in its window, or, when left owned the
// goal, one peer to report that. to is empty when no goal agent is involved.
func DepartureMail(left Peer, session string, others []Peer) (to []Handle, subject, body string) {
	for _, p := range others {
		if IsGoalAgent(p.Handle) {
			to = append(to, p.Handle)
		}
	}
	switch {
	case len(to) > 0:
		subject, body = GoalPeerLeft(left, session)
	case IsGoalAgent(left.Handle) && len(others) > 0:
		to = []Handle{reportingPeer(others).Handle}
		subject, body = GoalAgentLeft(left)
	}
	return to, subject, body
}

// reportingPeer picks who reports that a goal stopped: the first agent whose
// turn end reaches the user, else the first. peers is not empty.
func reportingPeer(peers []Peer) Peer {
	for _, p := range peers {
		if p.Kind.Notifies() {
			return p
		}
	}
	return peers[0]
}

// Briefing is what an agent's crew and role ask of it. A text left empty
// was not found; a path is set when the text was read from a file.
type Briefing struct {
	Crew, Conventions, ConventionsPath string
	Role, RolePrompt, RolePath         string
}

// MaxInline is the longest prompt a notice carries whole. A longer file is
// named instead, so the notice stays well inside what a harness accepts from
// a hook.
const MaxInline = 4000

// BriefNotice is the part of a start notice that carries the briefing; ""
// when there is none.
func BriefNotice(b Briefing) string {
	var parts []string
	if b.Conventions != "" {
		parts = append(parts, prompt(fmt.Sprintf("Team conventions of crew %q, shared by the agents in this Rex tab; where they differ from your role prompt, they win", b.Crew), b.Conventions, b.ConventionsPath))
	}
	if b.RolePrompt != "" {
		parts = append(parts, prompt("Your role here is "+b.Role, b.RolePrompt, b.RolePath))
	}
	return strings.Join(parts, "\n\n")
}

func prompt(title, text, path string) string {
	if len(text) > MaxInline && path != "" {
		return title + ": read " + path + " before you start."
	}
	return title + ":\n" + text
}
