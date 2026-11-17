package app

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

// Classify maps an error the app returns to its code in the closed set
// (fault). An error that already carries a code keeps it; the app's
// sentinels map here, the one place that knows them all; anything else is
// internal.
func Classify(err error) *fault.Error {
	if err == nil {
		return nil
	}
	if f, ok := fault.As(err); ok {
		return f
	}
	msg := err.Error()
	switch {
	case errors.As(err, new(trust.Untrusted)):
		f := fault.Wrap(err, fault.UntrustedFolder)
		untrustedNext(f, msg)
		return f
	case errors.As(err, new(trust.HooksUnapproved)):
		return fault.Wrap(err, fault.UnapprovedHooks).WithHint("the user approves the adapter's hooks with /hooks in a Codex session")
	case errors.Is(err, ErrUsage):
		return fault.Wrap(err, fault.Usage)
	case errors.Is(err, ErrNotInRex):
		return fault.Wrap(err, fault.FailedPrecondition).With("reason", "not_in_rex").
			WithHint("run this from an agent's pane in a Rex terminal")
	case errors.Is(err, ErrJobNotShown):
		return fault.Wrap(err, fault.FailedPrecondition).With("reason", "job_not_shown")
	case errors.Is(err, ErrNoView):
		return fault.Wrap(err, fault.FailedPrecondition).With("reason", "no_view")
	case errors.Is(err, ErrCrewTaken), errors.Is(err, store.ErrSessionTaken):
		return fault.Wrap(err, fault.Conflict)
	case errors.Is(err, rex.ErrNoBinary), errors.Is(err, rex.ErrUnreachable):
		return fault.Wrap(err, fault.RexUnavailable)
	case errors.Is(err, rex.ErrNotFound):
		return fault.Wrap(err, fault.NotFound).With("kind", "block")
	case errors.Is(err, amq.ErrNoBinary):
		return fault.Wrap(err, fault.AMQUnavailable)
	case strings.Contains(msg, "no AMQ identity"):
		return fault.Wrap(err, fault.NoIdentity)
	case errors.Is(err, ErrRefused):
		f := fault.Wrap(err, fault.PermissionDenied)
		if m := reasonRe.FindStringSubmatch(msg); m != nil {
			f.With("reason", m[1])
		} else {
			f.With("reason", "refused")
		}
		return f
	case errors.Is(err, context.DeadlineExceeded) && strings.HasPrefix(msg, "lock "):
		return fault.Wrap(err, fault.ResourceLocked).With("retry_after_ms", 1000)
	case errors.Is(err, context.DeadlineExceeded):
		return fault.Wrap(err, fault.Timeout)
	case errors.Is(err, context.Canceled):
		return fault.Wrap(err, fault.Interrupted)
	}
	return fault.Wrap(err, fault.Internal)
}

// reasonRe finds the gateway's reason, which refused() puts last in
// parentheses.
var reasonRe = regexp.MustCompile(`\(([a-z][a-z_]+)\)\S*$`)

// SummonRefusal is the code of a summon the guards refused, and the reason
// it names in details: a summon refusal has exited 3 since summon on send
// began, except the cap, which is cap_reached.
func SummonRefusal(reason string) *fault.Error {
	code, why := fault.PermissionDenied, "refused"
	switch {
	case strings.Contains(reason, trust.ReasonUnapprovedHooks):
		code, why = fault.UnapprovedHooks, "unapproved_hooks"
	case strings.Contains(reason, trust.ReasonUntrusted):
		code, why = fault.UntrustedFolder, "untrusted_folder"
	case strings.Contains(reason, "the cap"):
		code, why = fault.CapReached, "cap"
	case strings.HasPrefix(reason, "cycle"):
		why = "cycle"
	case strings.HasPrefix(reason, "too deep"):
		why = "too_deep"
	case strings.Contains(reason, "personal agent coordinates Epics"):
		why = "personal_summoner"
	case strings.Contains(reason, "delivers but never summons"):
		why = "kind_not_request"
	}
	f := fault.New(code, "%s", reason).With("reason", why)
	if code == fault.UntrustedFolder {
		untrustedNext(f, reason)
	}
	return f
}

var grantRe = regexp.MustCompile("rex-amq-adapter trust grant ([^`\\s]+)")

// untrustedNext names, as the next action, the trust grant the user runs
// themselves: never an agent.
func untrustedNext(f *fault.Error, msg string) {
	dir := "<dir>"
	if m := grantRe.FindStringSubmatch(msg); m != nil {
		dir = m[1]
	}
	f.WithHint("only the user grants folder trust: ask them to run the next action in their own terminal").
		WithNext("rex-amq-adapter trust grant "+dir).With("for", "user")
}
