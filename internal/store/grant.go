package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// An Epic's grant is notarized beside its other state, read-only (0444) and
// written once, so no agent of the run can edit what the user asked for:
//
//   - sessions/<session_id>/goal.md: the goal contract a personal agent piped
//     in (epic --goal -), when it has no file of its own.
//   - sessions/<session_id>/grant.json: the Grant record.
//   - sessions/<session_id>/grant.md: the same record for agents to read and
//     cite, with the user's words quoted verbatim.

// GoalPath is where the piped-in goal contract of session id is kept.
func (p Paths) GoalPath(id string) string { return filepath.Join(p.SessionDir(id), "goal.md") }

// GrantPath is where the readable grant of session id is kept.
func (p Paths) GrantPath(id string) string { return filepath.Join(p.SessionDir(id), "grant.md") }

func (p Paths) grantJSONPath(id string) string {
	return filepath.Join(p.SessionDir(id), "grant.json")
}

// Grant is what the Operator relayed when it opened an Epic: the user's own
// words (Asked, empty when it passed only a contract) and the goal contract
// (Contract, its absolute path, empty when there is none) with the SHA-256
// of the contract as read then. RelayedRole says who RelayedBy is
// (core.GrantRelayer): "" for a personal agent, as every grant recorded
// before the Operator was.
type Grant struct {
	Asked          string      `json:"asked"`
	RelayedBy      core.Handle `json:"relayed_by"`
	RelayedRole    string      `json:"relayed_role,omitempty"`
	RelayedAt      time.Time   `json:"relayed_at"`
	SessionID      string      `json:"session_id"`
	Contract       string      `json:"contract"`
	ContractSHA256 string      `json:"contract_sha256"`
}

// ContractDigest is the hex SHA-256 of a goal contract's text.
func ContractDigest(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// PutGoal keeps text as session id's goal contract, once: a goal already
// kept with the same text is left as it is, and one with other text is an
// error, never replaced.
func (s Store) PutGoal(id, text string) error {
	if id == "" || strings.TrimSpace(text) == "" {
		return errors.New("a goal needs its session and its text")
	}
	return writeOnce(s.P.GoalPath(id), []byte(text))
}

// PutGrant notarizes g as its session's grant, once: when one is kept
// already, that one is returned and g is dropped, so a retry never rewrites
// what was relayed or when. Both files are written read-only; a run that
// died between them has the missing one written from the kept record.
func (s Store) PutGrant(g Grant) (Grant, error) {
	if g.SessionID == "" || g.RelayedBy == "" || (g.Asked == "" && g.Contract == "") {
		return g, errors.New("a grant needs its session, who relayed it, and the user's words or a contract")
	}
	kept, ok, err := s.Grant(g.SessionID)
	if err != nil {
		return g, err
	}
	if !ok {
		b, err := json.Marshal(g)
		if err != nil {
			return g, err
		}
		if err := writeOnce(s.P.grantJSONPath(g.SessionID), append(b, '\n')); err != nil {
			return g, err
		}
		kept = g
	}
	if _, err := os.Stat(s.P.GrantPath(g.SessionID)); errors.Is(err, fs.ErrNotExist) {
		if err := writeOnce(s.P.GrantPath(g.SessionID), []byte(kept.Markdown())); err != nil {
			return kept, err
		}
	} else if err != nil {
		return kept, err
	}
	return kept, nil
}

// Grant reads session id's grant; ok is false when none was notarized.
func (s Store) Grant(id string) (Grant, bool, error) {
	var g Grant
	b, err := os.ReadFile(s.P.grantJSONPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return g, false, nil
	}
	if err != nil {
		return g, false, err
	}
	if err := json.Unmarshal(b, &g); err != nil {
		return g, false, fmt.Errorf("decode the grant of %s: %w", id, err)
	}
	return g, true, nil
}

// Markdown is g as grant.md: a header of its metadata, the user's words as
// a verbatim quote, and a pointer to the contract.
func (g Grant) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Grant for Epic session %s\n\n", g.SessionID)
	fmt.Fprintf(&b, "- relayed_by: %s (%s)\n", g.RelayedBy, core.GrantRelayer(g.RelayedRole))
	fmt.Fprintf(&b, "- relayed_at: %s\n", g.RelayedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- contract: %s\n", orNone(g.Contract))
	fmt.Fprintf(&b, "- contract_sha256: %s\n", orNone(g.ContractSHA256))
	b.WriteString("- recorded_by: rex-amq-adapter (read-only; written once when the Epic opened)\n\n")
	b.WriteString("## The user's words, relayed verbatim\n\n")
	if g.Asked == "" {
		b.WriteString("(none relayed: the contract is the whole grant)\n")
	} else {
		for _, line := range strings.Split(g.Asked, "\n") {
			if line == "" {
				b.WriteString(">\n")
				continue
			}
			b.WriteString("> " + line + "\n")
		}
	}
	b.WriteString("\n## Contract\n\n")
	if g.Contract == "" {
		b.WriteString("(none: the user's words above are the brief)\n")
	} else {
		fmt.Fprintf(&b, "Run the goal contract at %s; its SHA-256 when the Epic opened was %s.\n", g.Contract, g.ContractSHA256)
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// writeOnce creates path read-only (0444) holding data, whole or not at all:
// the data goes to a temporary file that is then linked into place, which
// fails when path exists. An existing path holding the same data is left as
// it is; one holding other data is an error.
func writeOnce(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil {
		if string(old) == string(data) {
			return nil
		}
		return fmt.Errorf("%s is already written, with other content, and is never replaced", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); errors.Is(err, fs.ErrExist) {
		return writeOnce(path, data) // another writer won: same data or an error
	} else if err != nil {
		return err
	}
	return nil
}
