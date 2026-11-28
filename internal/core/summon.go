package core

import (
	"fmt"
	"strings"
)

// Summon on send: an agent addresses a peer by handle or role, and the
// adapter delivers to it, resumes it, or starts it, inside fixed guards.
// The guards are the boundary (est9: trust the main agent; cap the count,
// refuse cycles, limit depth, keep it under the grant).

const (
	// SummonCap is how many live agents one Rex session may hold before a
	// summon refuses to resume or start another: a session is one task, and
	// six agents is what one person can still follow.
	SummonCap = 6
	// MaxDepth is how many summons may lie between the user and an agent:
	// the user's agents are 0, what they summon 1, and 2 is the end.
	MaxDepth = 2
)

// RequestKinds are the amq message kinds that may summon. A reply or an FYI
// delivers only.
var RequestKinds = map[string]bool{"question": true, "review_request": true, "todo": true, "brainstorm": true}

// SummonCheck is what the guards decide for a resume or start. A refusal
// is also an error, so a caller can carry it up.
type SummonCheck struct {
	Refused bool
	Reason  string
}

func (c SummonCheck) Error() string { return c.Reason }

// GuardSummoner refuses a summoner by its role, before anything is mailed or
// launched. A personal agent coordinates Epics only: its gateway lets it
// mail just a linked narrator, so an agent it summoned could never report
// back to it. It opens an Epic instead, whose orchestrator starts the crew.
func GuardSummoner(role string) SummonCheck {
	if role == PersonalRole {
		return SummonCheck{Refused: true, Reason: "a personal agent coordinates Epics only; open one with '" + EpicOpenCommand + "'"}
	}
	return SummonCheck{}
}

// GuardHubSummoner refuses a summon from a Hub session (C25): it holds
// personal agents only, so whatever it would start is work for an Epic.
func GuardHubSummoner(hub bool) SummonCheck {
	if hub {
		return SummonCheck{Refused: true, Reason: "this Rex session is a Hub, for personal agents only; multi-agent work opens an Epic: '" + EpicOpenCommand + "'"}
	}
	return SummonCheck{}
}

// EpicOpenCommand is how a personal agent opens an Epic.
const EpicOpenCommand = `rex-amq-adapter epic open <name> --cwd <dir> --asked "<the user's words>"`

// GuardSummon applies the guards to a summoner at depth, with chain its
// handles from the user's agent down to itself (itself last), wanting
// target (a handle, or a role), with live agents in the session. lead says
// the summoner leads its Epic: at depth 0 it may seat peers of its own
// read-only role (ReadOnly), a council or one seat at a time, so its own
// role is not a cycle then; its seats sit at depth 1, and from there any
// summon of the role is one again. A lead of a role that writes, or one
// naming its own handle, is still a cycle.
func GuardSummon(kind string, chain []Peer, depth int, target string, live int, lead bool) SummonCheck {
	if !RequestKinds[kind] {
		return SummonCheck{Refused: true, Reason: fmt.Sprintf("a %s delivers but never summons: only a request (question, review_request, todo, brainstorm) starts or resumes an agent", kind)}
	}
	for i, p := range chain {
		if lead && depth == 0 && i == len(chain)-1 && string(p.Handle) != target && ReadOnly(target) {
			continue // the lead seating a peer of its own read-only role
		}
		if string(p.Handle) == target || p.Role != "" && p.Role == target {
			return SummonCheck{Refused: true, Reason: fmt.Sprintf("cycle: %s is already in the summon chain %s", target, chainText(chain))}
		}
	}
	if depth+1 > MaxDepth {
		return SummonCheck{Refused: true, Reason: fmt.Sprintf("too deep: you are at depth %d and the limit is %d (the user's agents are 0)", depth, MaxDepth)}
	}
	if live >= SummonCap {
		return SummonCheck{Refused: true, Reason: fmt.Sprintf("the session already holds %d live agents, the cap; deliver to one of them or wait for one to finish", live)}
	}
	return SummonCheck{}
}

func chainText(chain []Peer) string {
	parts := make([]string, len(chain))
	for i, p := range chain {
		parts[i] = p.named()
	}
	return strings.Join(parts, " -> ")
}

// LooksLikeHandle reports whether s has a handle's shape, <kind>-<NN>, as
// opposed to a role.
func LooksLikeHandle(s string) bool {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	if _, err := ParseKind(s[:i]); err != nil {
		return false
	}
	return strings.Trim(s[i+1:], "0123456789") == ""
}
