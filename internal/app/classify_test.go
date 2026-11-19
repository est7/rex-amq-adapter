package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

// Every error the app returns maps into the closed set with the details a
// caller branches on; a code decided at the source survives a hint wrapped
// around it, and the message is the whole text.
func TestClassify(t *testing.T) {
	gateway := refusedSend(core.Endpoint{Handle: "claude-10", Role: "implementer"}, core.Endpoint{Handle: "claude-01", Role: core.PersonalRole},
		core.Decision{Reason: core.ReasonPersonalOnly})
	for _, c := range []struct {
		name    string
		err     error
		code    fault.Code
		details map[string]any
		next    string
	}{
		{"usage", fmt.Errorf("%w: bad", ErrUsage), fault.Usage, nil, ""},
		{"outside Rex", fmt.Errorf("x: %w", ErrNotInRex), fault.FailedPrecondition, map[string]any{"reason": "not_in_rex"}, ""},
		{"gateway refusal", refused("a to b", core.Decision{Reason: core.ReasonCrossEpic}), fault.PermissionDenied, map[string]any{"reason": string(core.ReasonCrossEpic)}, ""},
		{"gateway refusal with a hint", gateway, fault.PermissionDenied, map[string]any{"reason": string(core.ReasonPersonalOnly)}, ""},
		{"role refusal", fmt.Errorf("%w: only a personal agent opens Epics", ErrRefused), fault.PermissionDenied, map[string]any{"reason": "refused"}, ""},
		{"op id reused", errOpConflict("open", "op-1"), fault.Conflict, map[string]any{"reason": "op_id_reused"}, ""},
		{"no such agent", refusal(fault.NotFound, "not_in_session", "no agent x").With("kind", "agent"), fault.NotFound, map[string]any{"kind": "agent"}, ""},
		{"rex unreachable", fmt.Errorf("%w: down", rex.ErrUnreachable), fault.RexUnavailable, nil, ""},
		{"no rex", fmt.Errorf("%w: gone", rex.ErrNoBinary), fault.RexUnavailable, nil, ""},
		{"no amq", fmt.Errorf("%w: gone", amq.ErrNoBinary), fault.AMQUnavailable, nil, ""},
		{"no block", fmt.Errorf("%w: block:x", rex.ErrNotFound), fault.NotFound, map[string]any{"kind": "block"}, ""},
		{"untrusted", trust.Untrusted{Dir: "/w/app"}, fault.UntrustedFolder, map[string]any{"for": "user"}, "rex-amq-adapter trust grant"},
		{"timeout", fmt.Errorf("await: %w", context.DeadlineExceeded), fault.Timeout, nil, ""},
		{"lock", fmt.Errorf("lock lifecycle.lock: %w", context.DeadlineExceeded), fault.ResourceLocked, map[string]any{"retry_after_ms": 1000}, ""},
		{"human gate", humanRefused(GuardUnattended, "unattended"), fault.GuardDenied, map[string]any{"reason": "unattended"}, ""},
		{"no identity", errors.New("no AMQ identity for this pane"), fault.NoIdentity, nil, ""},
		{"anything else", errors.New("disk on fire"), fault.Internal, nil, ""},
	} {
		f := Classify(c.err)
		if f.Code != c.code || f.Message != c.err.Error() {
			t.Errorf("%s: %s %q, want %s with the whole message", c.name, f.Code, f.Message, c.code)
		}
		for k, v := range c.details {
			if f.Details[k] != v {
				t.Errorf("%s: details[%s] = %v, want %v", c.name, k, f.Details[k], v)
			}
		}
		if !strings.HasPrefix(f.NextAction, c.next) {
			t.Errorf("%s: next_action %q, want %q", c.name, f.NextAction, c.next)
		}
		if errors.Is(c.err, ErrRefused) && !errors.Is(f, ErrRefused) {
			t.Errorf("%s: the classified error no longer matches ErrRefused", c.name)
		}
	}
	if !strings.Contains(Classify(gateway).Message, "epic handback") {
		t.Error("the wrapped hint was lost")
	}
}
