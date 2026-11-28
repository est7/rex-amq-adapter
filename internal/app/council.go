package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A council (summon --council, after tstack's council skill) puts one
// request in front of one seat of a read-only role (core.ReadOnly) per
// harness of its routing: codex, agy and claude for the built-in reviewer.
// Each seat is the live agent of that harness in the target scope, a
// departed one resumed (only without --cwd), or a new one started; a caller
// of the role itself, working in the target directory, holds its own
// harness's seat (self): a design Epic's lead architect and two more make
// three. Every seat that needs a launch is
// checked first, and when any of them cannot be seated nothing is mailed or
// launched: the council reports the missing seats instead of substituting
// another harness. The request asks each seat for a score; a later council
// on the same scope reaches the seats that are still live, a new round.

// SeatResult is one council seat: its harness and what the summon did for it.
type SeatResult struct {
	Harness core.Kind          `json:"harness"`
	Action  string             `json:"action"` // self, deliver, resume, start
	Handle  core.Handle        `json:"handle,omitempty"`
	Block   string             `json:"block,omitempty"`
	Label   string             `json:"label,omitempty"`
	Mailed  bool               `json:"mailed"`
	Startup core.StartupStatus `json:"startup,omitempty"`
}

// seat is a council seat planned before anything is mailed or launched.
type seat struct {
	kind core.Kind
	live *agent      // deliver, or the caller itself (self)
	self bool        // the caller holds this seat
	plan *launchPlan // resume (plan.departed set) or start
}

// CouncilUsage is the usage error a council for target asks, or nil: only a
// read-only role, never a handle, and never with an explicit harness.
func CouncilUsage(target, harness string) error {
	switch {
	case core.LooksLikeHandle(target):
		return fmt.Errorf("%w: --council fans a read-only role out to its seats; %s names one agent", ErrUsage, target)
	case !core.ReadOnly(core.Role(target)):
		return fmt.Errorf("%w: --council is for a read-only role (reviewer, editor, qa, architect), which only reads the checkout; %q is not one", ErrUsage, target)
	case harness != "":
		return fmt.Errorf("%w: --council seats every harness of the role's routing; --harness names one, so use one or the other", ErrUsage)
	}
	return nil
}

func (e Env) council(ctx context.Context, loc Location, caller agent, r SummonRequest, role string) (SummonResult, error) {
	if err := CouncilUsage(r.Target, r.Harness); err != nil {
		return SummonResult{}, err
	}
	res := SummonResult{Action: "council", Parent: caller.Handle, Depth: caller.Depth + 1, DryRun: r.DryRun}
	e.preview = r.DryRun
	refuse := func(reason string) (SummonResult, error) {
		res.Action, res.Reason = "refused", reason
		e.summonLog(caller, r, res)
		return res, nil
	}
	s, err := e.FindSession(ctx, loc.Block)
	if err != nil {
		return res, err
	}
	dir := caller.Cwd
	if r.Cwd != "" {
		if info, err := os.Stat(r.Cwd); err != nil || !info.IsDir() {
			return refuse("--cwd " + r.Cwd + " is not a directory, so no seat was mailed or launched")
		}
		dir = r.Cwd
	}
	route, err := e.route(dir, role)
	if err != nil {
		return refuse(fmt.Sprintf("the routing of role %s cannot be read, so no seat was mailed or launched: %v", role, err))
	}
	recs, err := e.agents()
	if err != nil {
		return res, err
	}
	// The caller of the role, in the council's directory, holds its own
	// harness's seat; only its Epic's lead may seat peers of its own role
	// at all (leadSeats, core.GuardSummon), which reserve checks.
	self := caller.Role == role && (r.Cwd == "" || sameDir(r.Cwd, caller.Cwd))
	lead, err := e.leadSeats(caller, role, false)
	if err != nil {
		return res, err
	}
	var selfHandle core.Handle
	if self {
		selfHandle = caller.Handle
	}
	res.brief = councilBody(r.Body, role, route.Harnesses, selfHandle, caller.Kind)

	var seats []seat
	var missing []string
	starts := false
	for _, k := range route.Harnesses {
		if self && k == caller.Kind {
			seats = append(seats, seat{kind: k, live: &caller, self: true})
			continue
		}
		live, departed := seatsOf(recs, s, caller, role, k, r.Cwd)
		switch {
		case len(live) > 1:
			missing = append(missing, fmt.Sprintf("%s: %d live %s seats (%s); retire all but one", k, len(live), role, handleList(live)))
		case len(live) == 1:
			seats = append(seats, seat{kind: k, live: &live[0]})
		case departed != nil:
			if !e.installed(k) {
				missing = append(missing, fmt.Sprintf("%s: not installed", k))
				continue
			}
			p, refusal, err := e.planResume(ctx, caller, *departed)
			if err != nil {
				return res, err
			}
			if refusal != "" {
				missing = append(missing, fmt.Sprintf("%s: %s", k, refusal))
				continue
			}
			seats = append(seats, seat{kind: k, plan: &p})
		default:
			reason, err := e.ready(ctx, dir, k)
			if err != nil {
				return res, err
			}
			p, ok := e.planStart(caller, route, k, role, r.Subject, res.brief, dir)
			if reason == "" && !ok {
				reason = "the adapter cannot start this harness"
			}
			if reason != "" {
				missing = append(missing, fmt.Sprintf("%s: %s", k, reason))
				continue
			}
			seats = append(seats, seat{kind: k, plan: &p})
			starts = true
		}
	}
	if len(missing) > 0 {
		return refuse(fmt.Sprintf("the council of %s needs a seat on each of %s, and %s; no seat was mailed or launched, and no other harness stands in. Clear these, or summon single seats without --council", role, strings.Join(kindNames(route.Harnesses), ", "), strings.Join(missing, "; ")))
	}
	if starts {
		if refusal := startAuthorized(r); refusal != "" {
			return refuse(refusal + "; no seat was mailed or launched")
		}
	}
	var launches []*agent
	for _, st := range seats {
		if st.plan != nil {
			launches = append(launches, st.plan.departed)
		}
	}
	var tokens []string
	if len(launches) > 0 {
		chain, complete := e.chain(recs, caller)
		if !complete {
			return refuse("your summon chain cannot be checked: an agent that summoned you is no longer recorded, so no seat was mailed or launched")
		}
		if tokens, err = e.reserve(ctx, caller, s, r.Kind, chain, role, launches, lead, nil, r.DryRun); err != nil {
			var refusal core.SummonCheck
			if errors.As(err, &refusal) {
				return refuse(refusal.Reason + "; no seat was mailed or launched")
			}
			return res, err
		}
	}

	if r.DryRun {
		// Every seat planned, authorized, and within the cap: what the
		// council would do, with nothing mailed or launched.
		for _, st := range seats {
			res.Seats = append(res.Seats, plannedSeat(st))
		}
		return res, nil
	}
	// Mail first, as a single summon does: a resumed seat drains it.
	for _, st := range seats {
		out := plannedSeat(st)
		if out.Handle != "" && !st.self {
			if err := e.mailAs(ctx, caller.Handle, out.Handle, r.Kind, r.Subject, res.brief); err != nil {
				e.release(tokens)
				return e.councilPartial(caller, r, res, err)
			}
			out.Mailed = true
			res.Handles = append(res.Handles, out.Handle)
		}
		res.Seats = append(res.Seats, out)
	}
	next := 0
	for i, st := range seats {
		if st.plan == nil {
			continue
		}
		token := tokens[next]
		next++
		block, err := e.launchPlanned(ctx, caller, s, r.Split, *st.plan, token)
		if err != nil {
			e.release(tokens[next-1:])
			if reason, ok := launchRefusal(err); ok && next == 1 && !mailedAny(res.Seats) {
				// Refused under the lock before the first pane: nothing
				// was mailed or launched, so the council is refused whole.
				res.Seats = nil
				return refuse(reason + "; no seat was mailed or launched")
			}
			return e.councilPartial(caller, r, res, err)
		}
		res.Seats[i].Block = block
	}
	e.summonLog(caller, r, res)
	return res, nil
}

// mailedAny reports whether any of seats was mailed.
func mailedAny(seats []SeatResult) bool {
	for _, st := range seats {
		if st.Mailed {
			return true
		}
	}
	return false
}

// councilPartial logs a council that failed after some seats were mailed or
// launched, and returns which, with the failure.
func (e Env) councilPartial(caller agent, r SummonRequest, res SummonResult, err error) (SummonResult, error) {
	res.Reason = err.Error()
	e.summonLog(caller, r, res)
	return res, err
}

// seatsOf are the agents of role on kind that could hold its seat: the live
// ones in the scope (in cwd, or anywhere in the session without one), and,
// without cwd, the latest departed one of this session.
func seatsOf(recs []agent, s Session, caller agent, role string, kind core.Kind, cwd string) (live []agent, departed *agent) {
	for i, rec := range recs {
		if rec.Handle == caller.Handle || rec.Role != role || rec.Kind != kind {
			continue
		}
		switch {
		case rec.Live() && s.holds(rec) && (cwd == "" || sameDir(rec.Cwd, cwd)):
			live = append(live, rec)
		case !rec.Live() && cwd == "" && s.recorded(rec) && (departed == nil || rec.adoptedAt().After(departed.adoptedAt())):
			departed = &recs[i]
		}
	}
	return live, departed
}

// councilBody is the request every seat reads: the caller's, plus what makes
// it a council round, the seats and the score each owes, and the caller
// when it holds its own harness's seat (self, "" when it does not).
func councilBody(body, role string, kinds []core.Kind, self core.Handle, selfKind core.Kind) string {
	seated := ""
	if self != "" {
		seated = fmt.Sprintf(" %s, which sent it, holds the %s seat itself.", self, selfKind)
	}
	return body + fmt.Sprintf("\n\nCouncil review: this request went to one %s seat per harness (%s), each reading the same change on its own.%s Put `SCORE: <n>/10` in your reply, beside your verdict.", role, strings.Join(kindNames(kinds), ", "), seated)
}

func handleList(recs []agent) string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = string(r.Handle)
	}
	return strings.Join(out, ", ")
}

// AwaitCouncil waits, for each seat res launched and in parallel, up to
// timeout for its adoption, records how it started, and mails the council's
// request to each adopted seat that has no copy yet (a start; a resumed seat
// was mailed before its launch). res.Handles then names every seat with an
// agent.
func (e Env) AwaitCouncil(ctx context.Context, loc Location, r SummonRequest, res *SummonResult, timeout time.Duration) error {
	var wg sync.WaitGroup
	errs := make([]error, len(res.Seats))
	for i := range res.Seats {
		if res.Seats[i].Block == "" {
			continue
		}
		wg.Add(1)
		go func(st *SeatResult, err *error) {
			defer wg.Done()
			wctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			up, e2 := e.AwaitStartup(wctx, st.Block)
			if e2 != nil {
				*err = e2
				return
			}
			st.Startup = up.Status
			if up.Handle != "" {
				st.Handle = up.Handle
			}
		}(&res.Seats[i], &errs[i])
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	res.Handles = nil
	for i := range res.Seats {
		st := &res.Seats[i]
		if st.Handle == "" || st.Action == seatSelf {
			continue
		}
		if !st.Mailed && st.Startup == core.StartupAdopted {
			if err := e.Deliver(ctx, loc, st.Handle, r.Kind, r.Subject, res.brief); err != nil {
				return err
			}
			st.Mailed = true
		}
		res.Handles = append(res.Handles, st.Handle)
	}
	return nil
}

// seatSelf is the action of the seat the council's caller holds itself.
const seatSelf = "self"

// plannedSeat is what a seat would do: be held by the caller itself, deliver
// to its live agent, resume its departed one, or start one.
func plannedSeat(st seat) SeatResult {
	out := SeatResult{Harness: st.kind}
	switch {
	case st.self:
		out.Action, out.Handle = seatSelf, st.live.Handle
	case st.live != nil:
		out.Action, out.Handle = "deliver", st.live.Handle
	case st.plan.departed != nil:
		out.Action, out.Handle, out.Label = "resume", st.plan.departed.Handle, st.plan.label
	default:
		out.Action, out.Label = "start", st.plan.label
	}
	return out
}
