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
	// recorded, checked now; false when it changed, cannot be read, or the
	// grant names no contract or no digest to check.
	ContractSHA256OK bool   `json:"contract_sha256_ok"`
	ContractNote     string `json:"contract_note,omitempty"`
	Markdown         string `json:"markdown"`
}

// ReadGrant returns the grant of session (the caller's own when ""), for
// the agent in loc: the lead of that session (sessionLead), or a personal
// agent linked to it (core.LinkMatchesPersonal). Nothing is written.
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
	return e.readGrant(session)
}

// ReadGrantAsOperator returns the grant of session for the Operator
// (ResolveOperator): a personal agent only for an Epic linked to it.
func (e Env) ReadGrantAsOperator(getenv func(string) string, session string) (GrantRead, error) {
	op, err := e.ResolveOperator(getenv)
	if err != nil {
		return GrantRead{}, err
	}
	if op.Personal() {
		if err := e.mayReadGrant(*op.Agent, session); err != nil {
			return GrantRead{}, err
		}
	}
	return e.readGrant(session)
}

// readGrant is session's grant with its contract checked again now.
func (e Env) readGrant(session string) (GrantRead, error) {
	g, found, err := e.Store.Grant(session)
	if err != nil {
		return GrantRead{}, err
	}
	if !found {
		return GrantRead{}, fault.New(fault.NotFound, "Epic session %s has no notarized grant", session).With("kind", "session")
	}
	out := GrantRead{Grant: g, Markdown: g.Markdown()}
	out.ContractSHA256OK, out.ContractNote = contractCheck(g)
	return out, nil
}

// contractCheck reads g's contract now and checks it against the digest the
// grant recorded: ok only when both are there and agree.
func contractCheck(g store.Grant) (ok bool, note string) {
	switch {
	case g.Contract == "":
		return false, "the grant names no contract: the user's words are the whole grant"
	case g.ContractSHA256 == "":
		return false, "the grant recorded no digest of its contract"
	}
	b, err := os.ReadFile(g.Contract)
	switch {
	case err != nil:
		return false, fmt.Sprintf("the contract cannot be read: %v", err)
	case store.ContractDigest(b) != g.ContractSHA256:
		return false, "the contract changed since the Epic opened"
	}
	return true, ""
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
