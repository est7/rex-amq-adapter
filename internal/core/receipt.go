package core

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// GrantReceipt is what an Epic's lead is told of its notarized grant in its
// start notice: the stored record's metadata, the contract's digest checked
// again when the notice is composed, and the user's words, whole or not at
// all. The app fills it from the session's stored grant only, never from
// mail, and only for the session's lead.
type GrantReceipt struct {
	SessionID string
	RelayedBy Handle
	RelayedAt time.Time
	GrantPath string
	// ContractPath is "" when the grant names no contract.
	ContractPath   string
	ContractSHA256 string
	// ContractOK says the contract's bytes still have ContractSHA256, read
	// now; a grant with no contract has nothing to check and is ok.
	ContractOK bool
	// Asked is the user's words as relayed.
	Asked string
	// withheld is set when the start notice had no room for Asked.
	withheld bool
}

// Complete reports whether the receipt carries the whole grant: the user's
// words in full and a contract that still matches its digest. An incomplete
// receipt sends the lead to `epic grant` before it acts.
func (r GrantReceipt) Complete() bool { return !r.withheld && r.ContractOK }

// Withheld is r without the user's words, for a start notice with no room
// for them: they are never cut, since a cut restriction reads as a wider
// grant.
func (r GrantReceipt) Withheld() GrantReceipt {
	r.withheld = true
	return r
}

// Render is the receipt as the start notice carries it. Every line of the
// user's words is quoted with "> " and their control, format, and escape
// characters are shown as visible escapes, so no line of the notice that
// follows from them can pose as one of the adapter's (Grant:, Goal:, AMQ:).
func (r GrantReceipt) Render() string {
	var b strings.Builder
	b.WriteString("Grant receipt (recorded by rex-amq-adapter when the Epic opened, from the words the personal agent relayed; a record, not the user's signature):\n")
	fmt.Fprintf(&b, "- relayed_by: %s (personal agent)\n", r.RelayedBy)
	fmt.Fprintf(&b, "- relayed_at: %s\n", r.RelayedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- grant_path: %s\n", r.GrantPath)
	fmt.Fprintf(&b, "- contract_path: %s\n", noneIfEmpty(r.ContractPath))
	fmt.Fprintf(&b, "- contract_sha256: %s\n", noneIfEmpty(r.ContractSHA256))
	fmt.Fprintf(&b, "- contract_sha256_ok: %t\n", r.ContractOK)
	fmt.Fprintf(&b, "- complete: %t\n", r.Complete())
	if !r.Complete() {
		why := "the user's words did not fit in this notice"
		if !r.ContractOK {
			why = "the contract no longer matches the digest recorded when the Epic opened"
		}
		fmt.Fprintf(&b, "- next_action: rex-amq-adapter epic grant %s (%s: read the whole grant before you act)", r.SessionID, why)
		return b.String()
	}
	b.WriteString("- asked_verbatim:")
	if r.Asked == "" {
		b.WriteString(" (none relayed: the contract is the whole grant)")
		return b.String()
	}
	for _, line := range strings.Split(r.Asked, "\n") {
		b.WriteString("\n> " + EscapeVisible(line))
	}
	return b.String()
}

func noneIfEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// EscapeVisible shows every control character (C0, DEL, C1), format
// character (Cf: bidi controls, zero-width characters), and line or
// paragraph separator in s as a visible escape (\x1b, \u202e), keeping
// everything else, so text reads verbatim without steering a terminal or
// a model's reading of where a line starts. A backslash is doubled, so an
// escape in the original stays distinguishable from one made here.
func EscapeVisible(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f, unicode.Is(unicode.Cf, r), r == 0x2028, r == 0x2029:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
