package app

import (
	"fmt"
	"os"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// GrantRead is an Epic's notarized grant as `epic grant` returns it, with
// its contract's SHA-256 checked again now.
type GrantRead struct {
	store.Grant
	// ContractSHA256OK says the contract file still has the digest the grant
	// recorded; false when it changed or cannot be read, and true when the
	// grant names no contract.
	ContractSHA256OK bool   `json:"contract_sha256_ok"`
	ContractNote     string `json:"contract_note,omitempty"`
	Markdown         string `json:"markdown"`
}

// ReadGrant returns the grant of session (the caller's own when ""), for
// the agent in loc: the lead of that session, or a personal agent linked
// to it. Nothing is written.
func (e Env) ReadGrant(loc Location, session string) (GrantRead, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return GrantRead{}, err
	}
	if !ok || !rec.Live() || !placed(rec) {
		return GrantRead{}, fault.New(fault.NoIdentity, "no AMQ identity in a Rex session for this block (%s)", loc.Block)
	}
	if session == "" {
		session = rec.SessionID
	}
	if err := e.mayReadGrant(rec, session); err != nil {
		return GrantRead{}, err
	}
	g, found, err := e.Store.Grant(session)
	if err != nil {
		return GrantRead{}, err
	}
	if !found {
		return GrantRead{}, fault.New(fault.NotFound, "Epic session %s has no notarized grant", session).With("kind", "session")
	}
	out := GrantRead{Grant: g, ContractSHA256OK: true, Markdown: g.Markdown()}
	if g.Contract != "" {
		b, err := os.ReadFile(g.Contract)
		switch {
		case err != nil:
			out.ContractSHA256OK, out.ContractNote = false, fmt.Sprintf("the contract cannot be read: %v", err)
		case store.ContractDigest(b) != g.ContractSHA256:
			out.ContractSHA256OK, out.ContractNote = false, "the contract changed since the Epic opened"
		}
	}
	return out, nil
}

// mayReadGrant allows the lead of session (sessionLead) and a personal agent
// linked to it.
func (e Env) mayReadGrant(rec agent, session string) error {
	if session == rec.SessionID {
		lead, err := e.sessionLead(rec)
		if err != nil {
			return err
		}
		if lead {
			return nil
		}
		return fmt.Errorf("%w: only an Epic's lead, or a personal agent linked to it, reads its grant, and %s is %s", ErrRefused, rec.Handle, whoIs(rec))
	}
	if rec.Role != core.PersonalRole {
		return fmt.Errorf("%w: only a personal agent reads the grant of another session, and %s is %q", ErrRefused, rec.Handle, rec.Role)
	}
	if ok, err := e.personalLinked(rec, session); err != nil || ok {
		return err
	}
	return refused(fmt.Sprintf("%s reading the grant of %s", rec.Handle, session), core.Decision{Reason: core.ReasonNotLinked})
}
