// Package core holds the adapter's policy: which handle an agent gets, whether
// a doorbell may be typed, what to announce from a hook, and the text agents
// read. It takes observations and returns decisions; it performs no I/O.
package core

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// Kind is an agent harness the adapter can adopt.
type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
	// Agy is the Antigravity CLI.
	Agy Kind = "agy"
	// Pi is the pi coding agent.
	Pi Kind = "pi"
)

// InProcess reports whether the agent receives mail inside its own process
// (Pi's extension calls pi.sendMessage). Such an agent has no waker: nothing
// is ever typed into its terminal.
func (k Kind) InProcess() bool { return k == Pi }

// Notifies reports whether the agent's turn end can reach the user: only Claude
// Code and Codex have the notification hooks that carry its last message.
func (k Kind) Notifies() bool { return k == Claude || k == Codex }

// ParseKind accepts the --agent value hooks are installed with.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Claude, Codex, Agy, Pi:
		return k, nil
	}
	return "", fmt.Errorf("unsupported agent kind %q (want claude, codex, agy, or pi)", s)
}

// Handle is an AMQ agent handle. amq accepts [a-z0-9_-]+; the adapter also
// caps the length so a handle stays readable as a Rex block label.
type Handle string

var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func ParseHandle(s string) (Handle, error) {
	if !handleRe.MatchString(s) {
		return "", fmt.Errorf("invalid handle %q: want [a-z0-9][a-z0-9_-]{0,31}", s)
	}
	return Handle(s), nil
}

// DefaultBlockLabel is the label Rex gives every terminal block that was not
// labeled explicitly, including `rex run -- cmd` blocks.
const DefaultBlockLabel = "shell"

// NameHandle names an agent that holds no handle yet: <kind>-<NN>, with the
// lowest NN (from 01) not in taken. The kind is always the agent's own, so a
// handle never claims a harness it does not run on. The role is not part of
// the handle: it is the record's (store.Record.Role), so an agent can take a
// role, or change it, and keep its handle.
func NameHandle(kind Kind, taken map[Handle]bool) Handle {
	for n := 1; ; n++ {
		if h := Handle(fmt.Sprintf("%s-%02d", kind, n)); !taken[h] {
			return h
		}
	}
}

// Role reads an agent's role from its pane label: the part before the task
// (see PaneLabel), lowercased, runs of other characters folded to "-", and a
// trailing number dropped, so "Reviewer" and "reviewer-2" read as "reviewer".
// A label the adapter wrote reads as the role it shows: "architect(claude-04)
// | x" as "architect", "claude-04" as none. Rex's default label and a bare
// kind read as no role.
func Role(label string) string {
	head, _, _ := strings.Cut(label, "|")
	if _, role, ok := adapterHead(head); ok {
		return role
	}
	words := labelWords(head)
	if n := len(words); n > 0 && strings.Trim(words[n-1], "0123456789") == "" {
		words = words[:n-1]
	}
	if len(words) == 1 {
		if _, err := ParseKind(words[0]); err == nil || words[0] == DefaultBlockLabel {
			return ""
		}
	}
	return strings.Join(words, "-")
}

// InitialTask is the task a launcher wrote in a new agent's pane label,
// "<role> | <task>", so the agent starts with it. A label the adapter wrote
// (LauncherLabel is false) was an earlier agent's, and so was its task: none.
func InitialTask(label string) string {
	head, tail, ok := strings.Cut(label, "|")
	if !ok || !LauncherLabel(head) {
		return ""
	}
	return Task(tail)
}

// LauncherLabel reports whether a pane label was written by whoever started
// the agent ("reviewer | fix login", "orchestrator") rather than by the adapter for an
// agent in the pane before ("claude-04 | x", "architect(claude-04)"): a role
// named there is the new agent's to take.
func LauncherLabel(label string) bool {
	head, _, _ := strings.Cut(label, "|")
	_, _, adapter := adapterHead(head)
	return !adapter
}

// adapterHead recognizes the head of a pane label the adapter writes:
// "<kind>-<NN>", or "<role>(<kind>-<NN>)". It returns the handle and the
// role, "" for none.
func adapterHead(head string) (Handle, string, bool) {
	head = strings.TrimSpace(head)
	role := ""
	if open := strings.IndexByte(head, '('); open >= 0 && strings.HasSuffix(head, ")") {
		if role = strings.Join(labelWords(head[:open]), "-"); role == "" {
			return "", "", false
		}
		head = head[open+1 : len(head)-1]
	}
	kind, num, ok := strings.Cut(head, "-")
	if !ok || len(num) < 2 || strings.Trim(num, "0123456789") != "" {
		return "", "", false
	}
	if _, err := ParseKind(kind); err != nil {
		return "", "", false
	}
	return Handle(head), role, true
}

func labelWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// PaneSeparator divides an agent's pane label into its head and its task.
const PaneSeparator = " | "

// MaxTaskWidth bounds a task in terminal columns (a CJK character takes two),
// so the label stays readable in a narrow pane.
const MaxTaskWidth = 32

// PaneLabel is what an agent's pane is labelled: "<role>(<handle>)", or the
// handle without a role, then what it is doing when it has said so. The
// role leads, so a label cut short in a narrow pane still says what the agent
// is for; the handle follows in brackets, which is what peers mail.
func PaneLabel(h Handle, role, task string) string {
	head := PaneHead(h, role)
	if task == "" {
		return head
	}
	return head + PaneSeparator + task
}

// PaneHead is a pane label without its task.
func PaneHead(h Handle, role string) string {
	if role == "" {
		return string(h)
	}
	return role + "(" + string(h) + ")"
}

// Task is what an agent says it is doing, made fit for its pane label: one
// line, single spaces, no separator, at most MaxTaskWidth columns with an
// ellipsis when cut. Blank means no task.
func Task(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "|", "/")), " ")
	if columns(s) <= MaxTaskWidth {
		return s
	}
	used := 0
	for i, r := range s {
		if used+runeWidth(r) > MaxTaskWidth-1 { // one column for the ellipsis
			return strings.TrimRight(s[:i], " ") + "…"
		}
		used += runeWidth(r)
	}
	return s
}

func columns(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// runeWidth is a character's terminal width: two for East Asian wide and
// fullwidth characters (CJK, Hangul, most emoji), one otherwise. A sequence
// joined into one glyph (an emoji with a joiner) is counted per character, so
// it may be cut, or measured wider than it draws.
func runeWidth(r rune) int {
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	}
	return 1
}

// ShownTitle is a terminal title without the state glyph a harness puts in
// front of it ("✳ fix login" while Claude Code works): a first character that
// is neither a letter nor a digit, followed by a space.
func ShownTitle(title string) string {
	title = strings.TrimSpace(title)
	for i, r := range title {
		if i == 0 && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			if rest, ok := strings.CutPrefix(title[len(string(r)):], " "); ok {
				return strings.TrimSpace(rest)
			}
		}
		break
	}
	return title
}

// AgentViewTitle reports whether a terminal title is Claude Code's agent view
// (its list of background sessions), which shows no conversation: "claude
// agents", or "<n> awaiting input · claude agents" ("2 awaiting input · claude
// agents" observed; both forms read in Claude Code 2.1.318 by agy's review).
func AgentViewTitle(title string) bool {
	t := ShownTitle(title)
	if t == "claude agents" {
		return true
	}
	n, ok := strings.CutSuffix(t, " awaiting input · claude agents")
	return ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// TitleNamesHandle reports whether a title, without its state glyph, is h's
// pane label with any role and task, or none. The handle is unique, so a role
// or task changed since the harness last drew its title still names h.
func TitleNamesHandle(title string, h Handle) bool {
	if h == "" {
		return false
	}
	head, _, _ := strings.Cut(ShownTitle(title), PaneSeparator)
	if head == string(h) {
		return true
	}
	named, role, ok := adapterHead(head)
	return ok && role != "" && named == h
}
