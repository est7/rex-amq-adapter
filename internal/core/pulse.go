package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// A session's watcher tells its narrator when the session changed, so the
// narrator refreshes its view instead of polling. What it compares is the
// session's pulse: who is in it, what each is on, its newest mail, and what
// needs attention. The pulse holds envelopes and record state only, never a
// message body.

// PulseMember is one agent of a session as its pulse records it.
type PulseMember struct {
	Handle Handle `json:"handle"`
	Role   string `json:"role,omitempty"`
	Live   bool   `json:"live"`
	Task   string `json:"task,omitempty"`
	// LastMail is the newest message id the agent sent or received.
	LastMail string `json:"last_mail,omitempty"`
	// Attention lists what waits on someone, each "<what>@<since>": a turn
	// that ended in an error, a prompt waiting on the human, a stall.
	Attention []string `json:"attention,omitempty"`
}

// SessionPulse is a session's pulse.
type SessionPulse struct {
	Members []PulseMember `json:"members"`
}

// Normalize sorts p's members by handle and their attention, so equal
// sessions have equal pulses.
func (p SessionPulse) Normalize() SessionPulse {
	out := SessionPulse{Members: slices.Clone(p.Members)}
	for i := range out.Members {
		out.Members[i].Attention = slices.Clone(out.Members[i].Attention)
		sort.Strings(out.Members[i].Attention)
	}
	sort.Slice(out.Members, func(i, j int) bool { return out.Members[i].Handle < out.Members[j].Handle })
	return out
}

// Digest is a deterministic fingerprint of p.
func (p SessionPulse) Digest() string {
	b, _ := json.Marshal(p.Normalize())
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Priorities of a pulse notice, as amq takes them.
const (
	PriorityUrgent = "urgent"
	PriorityNormal = "normal"
	PriorityLow    = "low"
)

// PulseChange is what changed from prev to now, and how soon the narrator
// must hear it: urgent for attention (an agent left, a new error, stall, or
// wait on the human), which skips the narrator's doorbell hold; normal for a
// change of members or tasks; low for mail alone. Priority is "" when
// nothing changed.
type PulseChange struct {
	Priority string
	Lines    []string
}

// ComparePulse compares two pulses of one session.
func ComparePulse(prev, now SessionPulse) PulseChange {
	if prev.Digest() == now.Digest() {
		return PulseChange{}
	}
	before := map[Handle]PulseMember{}
	for _, m := range prev.Members {
		before[m.Handle] = m
	}
	var c PulseChange
	rank := map[string]int{"": 0, PriorityLow: 1, PriorityNormal: 2, PriorityUrgent: 3}
	raise := func(p, line string) {
		if rank[p] > rank[c.Priority] {
			c.Priority = p
		}
		c.Lines = append(c.Lines, line)
	}
	seen := map[Handle]bool{}
	for _, m := range now.Normalize().Members {
		seen[m.Handle] = true
		b, had := before[m.Handle]
		switch {
		case !had && m.Live:
			raise(PriorityNormal, fmt.Sprintf("%s joined", m.named()))
		case !had:
		case b.Live && !m.Live:
			raise(PriorityUrgent, fmt.Sprintf("%s left", m.named()))
		case !b.Live && m.Live:
			raise(PriorityNormal, fmt.Sprintf("%s is back", m.named()))
		}
		for _, a := range m.Attention {
			if !slices.Contains(b.Attention, a) {
				what, _, _ := strings.Cut(a, "@")
				raise(PriorityUrgent, fmt.Sprintf("%s: %s", m.Handle, strings.ReplaceAll(what, "_", " ")))
			}
		}
		if had && b.Task != m.Task && m.Live {
			raise(PriorityNormal, fmt.Sprintf("%s is on: %s", m.Handle, m.Task))
		}
		if had && b.Role != m.Role {
			raise(PriorityNormal, fmt.Sprintf("%s is now %s", m.Handle, m.Role))
		}
		if b.LastMail != m.LastMail && m.LastMail != "" {
			raise(PriorityLow, fmt.Sprintf("%s has new mail activity", m.Handle))
		}
	}
	for _, m := range prev.Normalize().Members {
		if !seen[m.Handle] && m.Live {
			raise(PriorityUrgent, fmt.Sprintf("%s left", m.named()))
		}
	}
	if c.Priority == "" {
		// Only something no line names changed (an attention cleared).
		raise(PriorityLow, "the session changed")
	}
	return c
}

func (m PulseMember) named() string {
	if m.Role == "" {
		return string(m.Handle)
	}
	return fmt.Sprintf("%s (%s)", m.Handle, m.Role)
}
