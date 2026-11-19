package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// probed is what the agent called name's tool shell read for ME_UNATTENDED
// (agentScript): raw, and after evaluating rex-amq-adapter env.
func (w *world) probed(name string) (raw, afterEnv, env string) {
	read := func(suffix string) string {
		b, _ := os.ReadFile(filepath.Join(w.dir, name+suffix))
		return string(b)
	}
	return read(".unattended"), read(".unattended-env"), read(".env")
}

func (w *world) wantProbe(name, want string) {
	w.t.Helper()
	raw, afterEnv, env := w.probed(name)
	if raw != want || afterEnv != want {
		w.t.Errorf("%s's tool shell read ME_UNATTENDED=%q (after env: %q), want %q; env printed %q\n%s", name, raw, afterEnv, want, env, w.dump())
	}
}

// record is what the adapter keeps of the agent called name: its identity,
// with the fields of its lease while it runs, else of its last placement
// (which carries departed_at).
func (w *world) record(name string) map[string]any {
	w.t.Helper()
	h := w.handleOf(name)
	read := func(dir string) (map[string]any, bool) {
		b, err := os.ReadFile(filepath.Join(w.home, dir, h+".json"))
		if os.IsNotExist(err) {
			return nil, false
		}
		if err != nil {
			w.t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			w.t.Fatal(err)
		}
		return m, true
	}
	r, _ := read("identities")
	place, live := read("leases")
	if !live {
		place, _ = r["last_placement"].(map[string]any)
	}
	for k, v := range place {
		r[k] = v
	}
	return r
}

// newTabWithEnv is newTab with assignments in the agent's own environment.
func (w *world) newTabWithEnv(name, role string, env ...string) *tab {
	w.t.Helper()
	var doc struct {
		Session string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(w.rex("new", "e2e-"+name, "-c", w.dir, "--json")), &doc); err != nil {
		w.t.Fatal(err)
	}
	args := w.start("run", name, role, "-s", doc.Session)
	for i, a := range args {
		if a == "--" {
			args = append(args[:i+1:i+1], append(append([]string{"env"}, env...), args[i+1:]...)...)
			break
		}
	}
	w.lastBlock = firstBlock(w, []byte(w.rex(args...)))
	return &tab{w: w, session: doc.Session, first: w.lastBlock}
}

// An Epic's agents run unattended in a private Rex server: the tool shells
// of its orchestrator, its narrator, a worker the orchestrator summons, and
// that worker resumed after it ended all read ME_UNATTENDED=1, which the
// test's own environment does not carry. A personal agent's shell reads
// none, and one that inherited the marker keeps it: env never sets or
// clears it there.
func TestAnEpicsAgentsRunUnattendedButNotPersonal(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("me", "personal")
	w.ready("me")
	w.wantProbe("me", "unset")
	w.newTabWithEnv("me2", "personal", "ME_UNATTENDED=1")
	w.ready("me2")
	w.wantProbe("me2", "1")
	for _, name := range []string{"me", "me2"} {
		if _, _, printed := w.probed(name); strings.Contains(printed, "ME_UNATTENDED") || !strings.HasPrefix(printed, "export AM_ROOT=") {
			t.Errorf("env for the personal agent %s: %q", name, printed)
		}
		if u, _ := w.record(name)["unattended"].(bool); u {
			t.Errorf("the personal agent %s is recorded unattended", name)
		}
	}

	w.typeInto(tb.first, "epic open e2e-unattended --cwd "+w.project("app")+" --asked open-it --timeout 40")
	w.until("the epic command to finish", 50*time.Second, func() bool { return strings.Contains(w.joined("me"), "exit=") })
	out := w.joined("me")
	if !strings.Contains(out, "exit=0") {
		t.Fatalf("epic:\n%s\n%s", out, w.dump())
	}
	var res struct {
		Orchestrator struct{ Block string } `json:"orchestrator"`
	}
	line, _, _ := strings.Cut(out, "\n")
	if err := envelopeData(line, &res); err != nil || res.Orchestrator.Block == "" {
		t.Fatalf("output %q: %v", out, err)
	}
	// Adoption, which epic awaits, comes before the probes.
	w.ready("orchestrator")
	w.ready("narrator")
	w.wantProbe("orchestrator", "1")
	w.wantProbe("narrator", "1")

	summons := func() int { return strings.Count(w.joined("orchestrator"), "exit=") }
	w.typeInto(res.Orchestrator.Block, "agent summon implementer --harness claude --asked start-a-worker --subject work --body e2e-worker")
	w.until("the summon to finish", 60*time.Second, func() bool { return summons() == 1 })
	if got := w.joined("orchestrator"); !strings.Contains(got, "exit=0") {
		t.Fatalf("summon:\n%s\n%s", got, w.dump())
	}
	w.ready("worker")
	w.wantProbe("worker", "1")
	if u, _ := w.record("worker")["unattended"].(bool); !u {
		t.Errorf("the worker is not recorded unattended: %v", w.record("worker"))
	}

	worker := w.handleOf("worker")
	block, _ := w.record("worker")["block_id"].(string)
	for _, f := range []string{"worker.ready", "worker.unattended", "worker.unattended-env", "worker.env"} {
		_ = os.Remove(filepath.Join(w.dir, f))
	}
	w.typeInto(block, "quit")
	w.until("the worker to depart", 15*time.Second, func() bool {
		_, gone := w.record("worker")["departed_at"]
		return gone
	})
	w.typeInto(res.Orchestrator.Block, "agent summon "+worker+" --subject again --body again")
	w.until("the resume to finish", 60*time.Second, func() bool { return summons() == 2 })
	if got := w.joined("orchestrator"); strings.Count(got, "exit=0") != 2 || !strings.Contains(got, `"action":"resume"`) {
		t.Fatalf("resume:\n%s\n%s", got, w.dump())
	}
	w.ready("worker")
	w.wantProbe("worker", "1")
	if h := w.handleOf("worker"); h != worker {
		t.Errorf("resumed as %s, want %s", h, worker)
	}
}
