package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// retireWorld is watcherWorld (orchestrator codex-01 in block:o, narrator
// codex-02, implementer codex-03 in block:w, Codex agents of session:s) with
// a second implementer codex-04 in block:v, and a waker for each implementer.
func retireWorld(t *testing.T) (Env, *fakeTerm, core.SessionKey, *amq.Fake) {
	t.Helper()
	e, term, key := watcherWorld(t)
	fake, ok := e.AMQ.(*amq.Fake)
	if !ok {
		t.Skip("observing wakers needs amq.Fake")
	}
	add(t, e, store.Record{Handle: "codex-04", Role: "implementer", BlockID: "block:v", Kind: core.Codex, Server: fakeServer, SessionID: "session:s", AgentPID: os.Getpid(), AdoptedAt: e.Now()})
	for _, h := range []core.Handle{"codex-03", "codex-04"} {
		if err := e.ensureWaker(context.Background(), recordOf(t, e, h)); err != nil {
			t.Fatal(err)
		}
		if st := fake.Waker(e.Paths.Root(), string(h)); st.Status == "missing" {
			t.Fatalf("the fixture's waker of %s is missing", h)
		}
	}
	return e, term, key, fake
}

func retire(e Env, by string, h core.Handle, force bool) (RetireResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.Retire(ctx, Location{Block: by}, RetireRequest{Handle: h, Force: force})
}

// unreadBodies is every unread message of h, whole.
func unreadBodies(t *testing.T, e Env, h core.Handle) []string {
	t.Helper()
	var out []string
	for _, id := range unread(t, e, string(h)) {
		b, err := os.ReadFile(filepath.Join(amq.Inbox(e.Paths.Root(), string(h)), id+".md"))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// Retiring a finished peer marks it departed for retired, retires its waker,
// and closes its block; the orchestrator gets no "left your Rex session"
// mail, then or when the agent's SessionEnd hook and collection run after the
// pane is gone. A peer whose pane was closed raw, the control, is parked as
// block_gone and its orchestrator is mailed.
func TestRetireClosesThePaneAndMailsNobody(t *testing.T) {
	e, term, _, fake := retireWorld(t)
	ctx := context.Background()
	res, err := retire(e, "block:o", "codex-03", false)
	if err != nil {
		t.Fatal(err)
	}
	if res != (RetireResult{Handle: "codex-03", Block: "block:w", Action: RetireRetired, DepartReason: store.DepartRetired}) {
		t.Errorf("result %+v", res)
	}
	if !slices.Equal(term.closed, []string{"block:w"}) {
		t.Errorf("closed %v, want block:w", term.closed)
	}
	if st := fake.Waker(e.Paths.Root(), "codex-03"); st.Status != "missing" {
		t.Errorf("the retired agent's waker still runs: %+v", st)
	}
	rec := recordOf(t, e, "codex-03")
	if rec.Live() || rec.DepartReason != store.DepartRetired || rec.Unfinished != "" {
		t.Fatalf("record: live=%v reason=%q unfinished=%q", rec.Live(), rec.DepartReason, rec.Unfinished)
	}
	at := *rec.DepartedAt
	// The dying agent's SessionEnd, then collection, which finds its block gone.
	if err := e.Depart(ctx, Location{Block: "block:w"}, HookInput{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	rec = recordOf(t, e, "codex-03")
	if rec.Live() || rec.DepartReason != store.DepartRetired || !rec.DepartedAt.Equal(at) {
		t.Errorf("a later SessionEnd or collection changed the retired record: %+v", rec)
	}
	if mail := unreadBodies(t, e, "codex-01"); len(mail) != 0 {
		t.Errorf("the orchestrator was mailed about a retired peer: %q", mail)
	}
	// The control: a raw close of codex-04's pane.
	if err := term.CloseBlock(ctx, "block:v"); err != nil {
		t.Fatal(err)
	}
	if err := e.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recordOf(t, e, "codex-04"); r.Live() || r.DepartReason == store.DepartRetired {
		t.Fatalf("the control was not parked as a crash: %+v", r)
	}
	mail := unreadBodies(t, e, "codex-01")
	if len(mail) != 1 || !strings.Contains(mail[0], "codex-04 (codex, implementer) left your Rex session: it quit, crashed") {
		t.Errorf("the control's departure mail: %q", mail)
	}
}

// The session watcher tells the narrator of a retired peer as routine
// (normal, "retired"), and of a raw-closed one as urgent ("left").
func TestRetireTellsTheNarratorRoutinely(t *testing.T) {
	e, term, key, _ := retireWorld(t)
	ctx := context.Background()
	keepers := []store.Record{recordOf(t, e, "codex-01"), recordOf(t, e, "codex-02")}
	look := func() []amq.Header {
		t.Helper()
		if err := e.watchOnce(ctx, key, keepers); err != nil {
			t.Fatal(err)
		}
		return narratorInbox(t, e)
	}
	if n := look(); len(n) != 0 {
		t.Fatalf("the first look told the narrator: %+v", n)
	}
	if _, err := retire(e, "block:o", "codex-03", false); err != nil {
		t.Fatal(err)
	}
	n := look()
	if len(n) != 1 || n[0].Priority != core.PriorityNormal {
		t.Fatalf("a retire's notice: %+v", n)
	}
	b, err := os.ReadFile(filepath.Join(amq.Inbox(e.Paths.Root(), "codex-02"), n[0].ID+".md"))
	if err != nil || !strings.Contains(string(b), "codex-03 (implementer) retired") || strings.Contains(string(b), "left") {
		t.Fatalf("a retire's notice: %s %v", b, err)
	}
	if err := term.CloseBlock(ctx, "block:v"); err != nil {
		t.Fatal(err)
	}
	n = look()
	if len(n) != 2 || n[1].Priority != core.PriorityUrgent {
		t.Fatalf("a raw close's notice: %+v", n)
	}
}

// A retire needs an adopted caller in the target's Rex session; it refuses
// the caller itself, a narrator, personal, or orchestrator agent, and, unless
// forced, a target that works or holds unread mail. A refused retire closes
// nothing and leaves the target live.
func TestRetireRefusesWithoutItsGuards(t *testing.T) {
	e, term, _, _ := retireWorld(t)
	add(t, e, store.Record{Handle: "codex-05", Role: "implementer", BlockID: "block:x", Kind: core.Codex, Server: fakeServer, SessionID: "session:other", AgentPID: os.Getpid(), AdoptedAt: e.Now()})
	add(t, e, store.Record{Handle: "claude-06", Role: core.PersonalRole, BlockID: "block:p", Kind: core.Claude, Server: fakeServer, SessionID: "session:s", AgentPID: os.Getpid(), AdoptedAt: e.Now()})
	for _, c := range []struct {
		name, by string
		h        core.Handle
		want     string
	}{
		{"no identity", "block:nobody", "codex-03", "no AMQ identity"},
		{"itself", "block:w", "codex-03", "does not retire itself"},
		{"another session", "block:o", "codex-05", "no agent codex-05 in your Rex session"},
		{"unknown", "block:o", "codex-99", "no agent codex-99"},
		{"narrator", "block:o", "codex-02", core.NarratorRole},
		{"personal", "block:o", "claude-06", core.PersonalRole},
		{"orchestrator", "block:w", "codex-01", core.OrchestratorRole},
	} {
		_, err := retire(e, c.by, c.h, true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.name, err, c.want)
		}
		if c.name != "no identity" && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v is not a refusal", c.name, err)
		}
	}
	term.states = map[string]rex.Status{"block:w": {State: "working"}}
	if _, err := retire(e, "block:o", "codex-03", false); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "codex-03: working") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("a working target: %v", err)
	}
	term.states = nil
	if err := amq.Send(context.Background(), e.AMQ, e.Paths.Root(), "codex-01", "codex-03", "more", "one more thing"); err != nil {
		t.Fatal(err)
	}
	if _, err := retire(e, "block:o", "codex-03", false); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "1 unread") {
		t.Errorf("a target with unread mail: %v", err)
	}
	if len(term.closed) != 0 || !recordOf(t, e, "codex-03").Live() {
		t.Fatalf("a refused retire closed %v or departed the target", term.closed)
	}
	if res, err := retire(e, "block:o", "codex-03", true); err != nil || res.Action != RetireRetired {
		t.Fatalf("forced: %+v %v", res, err)
	}
}

// Retiring an agent already departed is a named no-op that keeps its reason
// and closes nothing.
func TestRetireOfADepartedAgentIsANoOp(t *testing.T) {
	e, term, _, _ := retireWorld(t)
	leave(t, e, "codex-03")
	res, err := retire(e, "block:o", "codex-03", false)
	if err != nil {
		t.Fatal(err)
	}
	if res != (RetireResult{Handle: "codex-03", Block: "block:w", Action: RetireAlreadyDeparted, DepartReason: store.DepartSessionEnd}) {
		t.Errorf("result %+v", res)
	}
	if len(term.closed) != 0 {
		t.Errorf("closed %v", term.closed)
	}
	if _, err := retire(e, "block:o", "codex-04", false); err != nil {
		t.Fatal(err)
	}
	if res, err := retire(e, "block:o", "codex-04", false); err != nil || res.Action != RetireAlreadyDeparted || res.DepartReason != store.DepartRetired {
		t.Errorf("a second retire: %+v %v", res, err)
	}
}

// When the block does not close, the departure stands, nobody is mailed, and
// the error names the block for `rex block close`.
func TestRetireWhoseBlockDoesNotCloseStillDeparts(t *testing.T) {
	e, term, _, _ := retireWorld(t)
	term.closeErr = errors.New("rex timed out")
	res, err := retire(e, "block:o", "codex-03", false)
	if err == nil || !strings.Contains(err.Error(), "rex block close block:w") || !strings.Contains(err.Error(), "rex timed out") {
		t.Fatalf("error %v, want one naming `rex block close block:w`", err)
	}
	if res.Action != RetireRetired || res.Block != "block:w" {
		t.Errorf("result %+v", res)
	}
	if r := recordOf(t, e, "codex-03"); r.Live() || r.DepartReason != store.DepartRetired {
		t.Errorf("the departure did not stand: %+v", r)
	}
	if mail := unreadBodies(t, e, "codex-01"); len(mail) != 0 {
		t.Errorf("the orchestrator was mailed: %q", mail)
	}
}
