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
