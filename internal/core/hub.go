package core

import "strings"

// A Hub is where the user's personal agents work: one canonical Hub
// directory (internal/hub resolves them) on one Rex server. Every personal
// agent of a Hub shares its Epics: a link names the Hub, not the personal
// agent that made it, so any live personal tab of the Hub, in any Rex
// session, reaches the linked narrators and may close their Epics.

// HubKey names a Hub: "<server>|<canonical dir>". The app computes it from
// a session's recorded policy, never from an environment variable or a mail
// header.
type HubKey string

// NewHubKey is the key of the Hub dir (canonical) on server.
func NewHubKey(server, dir string) HubKey {
	if dir == "" {
		return ""
	}
	return HubKey(server + "|" + dir)
}

// Server is the Rex server of k.
func (k HubKey) Server() string {
	s, _, _ := strings.Cut(string(k), "|")
	return s
}

// Dir is the canonical Hub directory of k.
func (k HubKey) Dir() string {
	_, d, _ := strings.Cut(string(k), "|")
	return d
}

// LinkMatchesPersonal reports whether link l grants personal agent p (an
// endpoint the app built: its Hub is set only for a live personal agent of a
// Hub session) the channel to l's narrator: p is a member of l's Hub, or l
// is a session-keyed link of p's own session (one made before links named
// Hubs, or by a personal agent of no Hub). Every decision about a personal
// agent and an Epic (the gateway, epic list, show, close, link, unlink,
// grant) asks this one predicate.
func LinkMatchesPersonal(l Link, p Endpoint) bool {
	return (l.Hub != "" && p.Hub == l.Hub) || (l.Personal.Placed() && l.Personal == p.Session())
}

// Placed reports whether k names a session on a server.
func (k SessionKey) Placed() bool { return k.Server != "" && k.SessionID != "" }
