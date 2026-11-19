package core

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Peer is a live agent as the session policy sees it. Role is its record's,
// "" for none.
type Peer struct {
	Handle    Handle
	Kind      Kind
	Role      string
	Block     string
	Window    string
	Parent    Handle
	AdoptedAt time.Time
	// Lead marks the agent that leads its Epic session (the app's
	// sessionLead), whatever its role: who hears departures.
	Lead bool
}

// named is how a notice names p: its handle, which is what peers mail, then
// its kind and role.
func (p Peer) named() string {
	if p.Role == "" {
		return fmt.Sprintf("%s (%s)", p.Handle, p.Kind)
	}
	return fmt.Sprintf("%s (%s, %s)", p.Handle, p.Kind, p.Role)
}

// SessionMates are the peers whose blocks are in the session, other than any
// in self's own block (an agent this one replaced there). windowOf places
// every block of the session; a peer whose block it does not list is
// elsewhere. A Rex session holds one task, whose agents may fill several tabs
// (a tab is a team), so these are the agents "beside" this one. A Stealth
// agent (a narrator, a personal agent) is nobody's mate.
func SessionMates(selfBlock string, windowOf map[string]string, peers []Peer) []Peer {
	var out []Peer
	for _, p := range peers {
		if _, placed := windowOf[p.Block]; placed && p.Block != selfBlock && !Stealth(p.Role) {
			out = append(out, p)
		}
	}
	return out
}

// MatesNotice names an agent's session-mates; "" when it has none.
func MatesNotice(mates []Peer) string {
	if len(mates) == 0 {
		return ""
	}
	names := make([]string, len(mates))
	for i, m := range mates {
		names[i] = m.named()
	}
	return "Agents beside you in this Rex session: " + strings.Join(names, ", ") +
		". Reach them with `rex-amq-adapter mail send --to <handle>`."
}

// JoinedNotice tells a session-mate that p started beside it.
func JoinedNotice(p Peer) string {
	return fmt.Sprintf("AMQ: %s joined your Rex session. Reach it with `rex-amq-adapter mail send --to %s`.", p.named(), p.Handle)
}

// OrchestratorNotice tells an agent that an orchestrator among mates
// coordinates its session, so a question, report, or decision its texts would
// take to the human goes to that orchestrator instead; "" when mates hold
// none. The orchestrator's role prompt says the same from its side, and it
// alone says what still waits for the human.
func OrchestratorNotice(mates []Peer) string {
	var orchs []string
	for _, m := range mates {
		if m.Role == OrchestratorRole {
			orchs = append(orchs, string(m.Handle))
		}
	}
	var to string
	switch len(orchs) {
	case 0:
		return ""
	case 1:
		to = orchs[0]
	default:
		to = fmt.Sprintf("the orchestrator that briefed you or your lead (one of %s; if neither did, %s)", strings.Join(orchs, ", "), orchs[0])
	}
	return fmt.Sprintf("An orchestrator coordinates this session. Where your crew's conventions, your role prompt, your brief, or the project's instructions (AGENTS.md, CLAUDE.md) would take a question, a report, or a decision to the human or user, send it to %s instead; this overrides those texts. Keep your crew's own report chain: only what would reach the human goes there. What still waits for the human is the orchestrator's to say, and a permission or trust dialog always does: nobody approves it or works around it for them.", to)
}

// GrantNotice tells an Epic's lead where its notarized grant is: the goal
// contract it runs (contract, "" when it has none) and grant.md, which
// quotes the user's words as the personal agent by relayed them.
func GrantNotice(contract, grant string, by Handle) string {
	line := fmt.Sprintf("Grant: %s (user's words relayed verbatim by personal %s, recorded by rex-amq-adapter)", grant, by)
	if contract == "" {
		return line
	}
	return "Goal: " + contract + "\n" + line
}

// LeftNotice tells a session-mate that p's agent session ended. Its handle stays
// reserved for a resume, so mail still reaches it then.
func LeftNotice(p Peer) string {
	return fmt.Sprintf("AMQ: %s left your Rex session. Mail to it waits until its session resumes; do not wait for its reply.", p.named())
}

// UndeliveredNotice tells a sender that its mail went to to, an agent that had
// left. occupant is the agent now in to's pane, if any; mates are the sender's
// session-mates, the agents it may have meant.
func UndeliveredNotice(to Handle, occupant *Peer, mates []Peer) string {
	notice := fmt.Sprintf("AMQ: your mail to %s will not be read unless its session resumes: that agent left.", to)
	if occupant != nil {
		notice += fmt.Sprintf(" %s now runs in its pane.", occupant.named())
	}
	if beside := MatesNotice(mates); beside != "" {
		return notice + " " + beside
	}
	return notice + " Run `rex-amq-adapter agent list` for the agents in your Rex session."
}

// OrchestratorRole is the pane label of the agent that owns a goal. A session holding
// one routes to it what its other agents would take to the user.
const OrchestratorRole = "orchestrator"

// OrchestratorPeerLeft is the mail telling an orchestrator that p departed. session is
// p's agent session, "" when unknown.
func OrchestratorPeerLeft(p Peer, session string) (subject, body string) {
	subject = fmt.Sprintf("%s left your Rex session", p.Handle)
	body = fmt.Sprintf("%s left your Rex session: it quit, crashed, or its pane closed. You own the goal, so decide what happens next: resume it, start a new agent, or change the plan.", p.named())
	if session != "" {
		body += fmt.Sprintf(" To keep its context, resume session %s in a pane; it takes its handle back.", session)
	}
	return subject, body + " It cannot read a reply to this mail."
}

// OrchestratorLeft is the mail telling a surviving agent that the orchestrator
// departed.
func OrchestratorLeft(p Peer) (subject, body string) {
	return fmt.Sprintf("%s left your Rex session", p.Handle), fmt.Sprintf("%s, which owned this session's goal, left your Rex session: it quit, crashed, or its pane closed, and nobody is running the goal. Finish the step you are on, say in your final message that the goal stopped and where the work stands, then end your turn: do not take over the goal. It cannot read a reply to this mail.", p.Handle)
}

// DepartureMail says who hears at once that left departed: the lead among
// others, the other live agents in its session (Peer.Lead), or, when left
// led the goal, one peer to report that. to is empty when no lead is
// involved.
// A Stealth agent's departure is nobody's mail, and none is told to one: the
// session watcher tells a narrator itself.
func DepartureMail(left Peer, session string, others []Peer) (to []Handle, subject, body string) {
	if Stealth(left.Role) {
		return nil, "", ""
	}
	others = slices.DeleteFunc(slices.Clone(others), func(p Peer) bool { return Stealth(p.Role) })
	for _, p := range others {
		if p.Lead {
			to = append(to, p.Handle)
		}
	}
	switch {
	case len(to) > 0:
		subject, body = OrchestratorPeerLeft(left, session)
	case left.Lead && len(others) > 0:
		to = []Handle{reportingPeer(others).Handle}
		subject, body = OrchestratorLeft(left)
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
	// NameConventions and NameRole name a prompt instead of inlining it. The
	// start notice's allocator sets them as it makes room; a prompt longer
	// than MaxInline is named whatever they say.
	NameConventions, NameRole bool
}

// MaxInline is the longest prompt a notice carries whole. A longer prompt is
// named instead, so the notice stays well inside what a harness accepts from
// a hook.
const MaxInline = 4000

// BriefNotice is the part of a start notice that carries the briefing; ""
// when there is none.
func BriefNotice(b Briefing) string { return briefNotice(b) }

// briefNotice is the briefing as the notice carries it: a prompt inlined
// while it is within MaxInline, a pointer to its file or, for a built-in
// prompt with no file, to the command that prints it.
func briefNotice(b Briefing) string {
	var parts []string
	if b.Conventions != "" {
		parts = append(parts, prompt(fmt.Sprintf("Team conventions of crew %q, shared by the agents in this Rex session; where they differ from your role prompt, they win", b.Crew), b.Conventions, b.ConventionsPath, b.NameConventions, conventionsPointer(b.Crew)))
	}
	if b.RolePrompt != "" {
		parts = append(parts, prompt("Your role here is "+b.Role, b.RolePrompt, b.RolePath, b.NameRole, rolePointer(b.Crew, b.Role)))
	}
	return strings.Join(parts, "\n\n")
}

// conventionsPointer is how the notice names crew crew's conventions when
// it does not inline them: their file, or for a built-in crew the command
// that prints them.
func conventionsPointer(crew string) string {
	name := crew
	if name == "" {
		name = "<crew>"
	}
	return "rex-amq-adapter crew show " + name + " --text"
}

// rolePointer is how the notice names role's prompt when it does not inline
// it.
func rolePointer(crew, role string) string {
	if crew == "" {
		return "rex-amq-adapter crew show --role " + role + " --text"
	}
	return "rex-amq-adapter crew show " + crew + " --role " + role + " --text"
}

func prompt(title, text, path string, name bool, pointer string) string {
	if !name && len(text) <= MaxInline {
		return title + ":\n" + text
	}
	if path != "" {
		return title + ": read " + path + " before you start."
	}
	return title + ": run `" + pointer + "` before you start."
}

// ContextCap is the most a hook may give Claude Code as context: a longer
// value is replaced by a file path and a 2,000-character preview, and what
// follows the preview never reaches the model. Lengths count UTF-16 code
// units (ContextLen), as Claude Code's JavaScript does.
const ContextCap = 10000

// StartBudget is what a start notice may take of ContextCap, leaving the rest
// for the join notes and mail line the same hook reply can carry. It is the
// start notice's share when those fit; HookReply keeps the whole reply within
// ContextCap when they do not.
const StartBudget = ContextCap - 1000

// ContextLen is the length of s as a harness written in JavaScript counts
// it: UTF-16 code units.
func ContextLen(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// StartParts is everything a start notice carries, one part each, so the
// allocator can decide what a notice over StartBudget gives up.
type StartParts struct {
	// Head is the identity and mail commands; it always stays inline.
	Head string
	// Mates names the session-mates (MatesNotice); Task is the pane-label
	// line (TaskNotice).
	Mates, Task string
	// Briefing is the crew's conventions and the role's prompt.
	Briefing Briefing
	// Receipt is an Epic lead's grant receipt; every other agent has none.
	Receipt *GrantReceipt
	// Tail is who stands for the human, what waits on the human, and the
	// lead's Goal and Grant lines; it always stays inline, last.
	Tail string
}

// StartNotice is the one allocator of a start notice's StartBudget: head,
// mates, task, briefing, grant receipt, tail, in that order. Head and tail
// always stay inline, since an agent cannot do without them. What does not
// fit is given up in this order: the longer prompt becomes a pointer, then
// the other, then the mates list becomes a count, then the receipt leaves
// out the user's words (complete: false, pointing at `epic grant`; it never
// carries a part of them). A notice still over budget then (no input this
// adapter writes gets there) is cut before its tail.
func StartNotice(p StartParts) string { return startNoticeWithin(p, StartBudget) }

// startSteps are what a start notice gives up, in order; the first gives up
// nothing.
func startSteps(p StartParts) []func(*StartParts) {
	conventionsFirst := ContextLen(p.Briefing.Conventions) >= ContextLen(p.Briefing.RolePrompt)
	return []func(*StartParts){
		func(*StartParts) {},
		func(q *StartParts) {
			q.Briefing.NameConventions, q.Briefing.NameRole = conventionsFirst, !conventionsFirst
		},
		func(q *StartParts) { q.Briefing.NameConventions, q.Briefing.NameRole = true, true },
		func(q *StartParts) { q.Mates = MatesCount(q.Mates) },
		func(q *StartParts) {
			if q.Receipt != nil {
				r := q.Receipt.Withheld()
				q.Receipt = &r
			}
		},
	}
}

// startNoticeWithin is StartNotice within budget units.
func startNoticeWithin(p StartParts, budget int) string {
	var s string
	for _, step := range startSteps(p) {
		step(&p)
		if s = p.compose(); ContextLen(s) <= budget {
			return s
		}
	}
	tail := p.Tail
	p.Tail = ""
	sep := ""
	if tail != "" {
		sep = "\n\n"
	}
	return cut(p.compose(), budget-ContextLen(sep+tail)) + sep + tail
}

// startFloor is the length of p's start notice once it gave up everything
// it may give up: the least it needs whole.
func startFloor(p StartParts) int {
	for _, step := range startSteps(p) {
		step(&p)
	}
	return ContextLen(p.compose())
}

// HookReply is a hook's whole context: the start notice of start (nil for
// none), the window notes queued for the agent (oldest first), and the mail
// notice, space-joined, within budget (at most ContextCap; the caller keeps
// out of it what it appends after). The mail and the notes take their room
// first, and the start notice is composed within what they leave, at most
// StartBudget, giving up its parts in StartNotice's order: its prompts
// become pointers before a grant receipt that does not fit leaves out the
// user's words whole (complete: false), never a part of them. Notes too
// many for even the least start notice (startFloor) give way oldest first,
// to one line that counts them (WindowNotesCount).
func HookReply(budget int, start *StartParts, notes []string, mail string) string {
	budget = min(budget, ContextCap)
	floor := 0
	if start != nil {
		floor = startFloor(*start) + 1
	}
	rest := func(kept []string) string {
		return joinSpaced(append(append([]string{WindowNotesCount(len(notes) - len(kept))}, kept...), mail)...)
	}
	kept := notes
	for len(kept) > 0 && floor+ContextLen(rest(kept)) > budget {
		kept = kept[1:]
	}
	tail := rest(kept)
	out := tail
	if start != nil {
		out = joinSpaced(startNoticeWithin(*start, min(StartBudget, budget-ContextLen(tail)-1)), tail)
	}
	if ContextLen(out) <= budget {
		return out
	}
	// Only a mail notice near the budget by itself gets here (none this
	// adapter writes does); the start notice in it was composed within what
	// was left, so no receipt in it carries the user's words.
	return cut(out, budget)
}

// WindowNotesCount stands for n window notes a hook reply had no room for;
// "" for none.
func WindowNotesCount(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("AMQ: %d earlier note(s) about agents joining or leaving your Rex session did not fit here: `rex-amq-adapter agent list` names who is here now.", n)
}

// joinSpaced joins the non-empty notices with a space.
func joinSpaced(notices ...string) string {
	var out []string
	for _, n := range notices {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// compose renders p's parts in the order an agent reads them: the head, its
// mates and task lines, then the briefing, receipt, and tail as paragraphs.
func (p StartParts) compose() string {
	head := p.Head
	if p.Mates != "" {
		head += " " + p.Mates
	}
	if p.Task != "" {
		head += "\n" + p.Task
	}
	parts := []string{head, briefNotice(p.Briefing)}
	if p.Receipt != nil {
		parts = append(parts, p.Receipt.Render())
	}
	parts = append(parts, p.Tail)
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

// cut shortens s to at most n UTF-16 code units, never inside a rune,
// marking a cut with an ellipsis.
func cut(s string, n int) string {
	if ContextLen(s) <= n {
		return s
	}
	used := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if used+w > n-1 {
			return s[:i] + "…"
		}
		used += w
	}
	return s
}

// MatesCount is a mates line cut to the number of agents it names and the
// command that lists them; "" stays "".
func MatesCount(mates string) string {
	if mates == "" {
		return ""
	}
	list, _, _ := strings.Cut(strings.TrimPrefix(mates, "Agents beside you in this Rex session: "), ". Reach them")
	n := len(strings.Split(list, "), "))
	return fmt.Sprintf("%d agent(s) are beside you in this Rex session: `rex-amq-adapter agent list` names them.", n)
}
