package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A Rex server restart, in a private server: the agents' processes die with
// it and their tabs come back as login shells (session and block ids kept, a
// new server instance). Starting a conversation again in its pane, as a user
// or rex-durable's reopen would, takes its handle back.

// leaseOf is the lease file of handle h, or nil while it runs nowhere.
func (w *world) leaseOf(h string) map[string]any {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.home, "leases", h+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		w.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		w.t.Fatal(err)
	}
	return m
}

// fenceOf is the gateway fence of handle h: its session and generation.
func (w *world) fenceOf(h string) (session string, generation int) {
	w.t.Helper()
	b, err := os.ReadFile(filepath.Join(w.home, "identities", h+".json"))
	if err != nil {
		w.t.Fatal(err)
	}
	var id struct {
		Fence struct {
			SessionID  string `json:"session_id"`
			Generation int    `json:"generation"`
		} `json:"fence"`
	}
	if err := json.Unmarshal(b, &id); err != nil {
		w.t.Fatal(err)
	}
	return id.Fence.SessionID, id.Fence.Generation
}

func (w *world) instanceID() string {
	w.t.Helper()
	var who struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(w.rex("whoami", "--json")), &who); err != nil {
		w.t.Fatal(err)
	}
	return who.InstanceID
}

func (w *world) blocksOf(session string) []string {
	w.t.Helper()
	var doc struct {
		Blocks []struct {
			ID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(w.rex("block", "ls", "-s", session, "--json")), &doc); err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, b := range doc.Blocks {
		out = append(out, b.ID)
	}
	return out
}

// hookOutput is what the scripted agent called name got from its hooks.
func (w *world) hookOutput(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.dir, name+".hook"))
	return string(b)
}

// relaunch starts the scripted agent called name again in block, whose
// agent died with the server: the block runs a login shell now, and the
// agent's command is typed into it, as rex-durable's reopen does.
func (w *world) relaunch(block, name string) {
	w.t.Helper()
	_ = os.Remove(filepath.Join(w.dir, name+".ready"))
	w.until("a shell in the restored "+block, 15*time.Second, func() bool {
		out, err := w.rexErr("block", "call", "-b", block, "process")
		return err == nil && strings.Contains(out, `"foreground":{"pid"`)
	})
	w.typeInto(block, "exec /bin/sh '"+filepath.Join(w.dir, "agent.sh")+"' "+name+" claude")
	w.lastBlock = block
	w.ready(name)
}

func (w *world) doorbells(name string) int { return strings.Count(w.got(name), "AMQ doorbell") }

func asInt(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// A normal restart keeps the Rex session and its blocks: the orchestrator of
// an Epic, started again in its pane, is the same handle in the same session
// and block under the next lease epoch, with its unread mail kept and told,
// the Epic's lifecycle untouched, and doorbells reaching it again through the
// handle's waker. The mail comes from a peer in the Epic's session: the
// gateway fences the personal agent off from the orchestrator.
func TestRexRestartKeepsTheHandleMailAndEpic(t *testing.T) {
	w := newRestartableWorld(t)
	tb := w.newTab("me", "personal")
	w.ready("me")
	w.typeInto(tb.first, "epic e2e-restart -c "+w.project("app")+" --asked open-it --timeout 40")
	w.until("the epic command to finish", 50*time.Second, func() bool { return strings.Contains(w.joined("me"), "exit=") })
	if out := w.joined("me"); !strings.Contains(out, "exit=0") {
		t.Fatalf("epic:\n%s\n%s", out, w.dump())
	}
	w.ready("orchestrator")
	orch := w.handleOf("orchestrator")
	before := w.leaseOf(orch)
	session, block := before["session_id"].(string), before["block_id"].(string)
	epic := &tab{w: w, session: session, first: block}
	epic.split("peer", "implementer")
	w.ready("peer")
	peer := w.handleOf("peer")
	lifecycles, _ := filepath.Glob(filepath.Join(w.root(), "extensions", "rex-amq-adapter", "sessions", "*", "lifecycle.json"))
	if len(lifecycles) != 1 {
		t.Fatalf("lifecycles %v\n%s", lifecycles, w.dump())
	}
	lifecycle, err := os.ReadFile(lifecycles[0])
	if err != nil || !strings.Contains(string(lifecycle), session) || !strings.Contains(string(lifecycle), `"opened"`) {
		t.Fatalf("the Epic's lifecycle: %s %v", lifecycle, err)
	}
	// Mail that waits through the restart: its doorbell rang, and nobody drains.
	w.send(peer, orch, "before the restart")
	w.until("the first doorbell", 20*time.Second, func() bool { return w.doorbells("orchestrator") >= 1 })
	wakerBefore := w.wakerPID(orch)
	instance := w.instanceID()

	w.restartServer(true)
	if got := w.instanceID(); got == instance || got == "" {
		t.Fatalf("the server instance after a restart: %q (was %q)", got, instance)
	}
	if blocks := w.blocksOf(session); !strings.Contains(strings.Join(blocks, " "), block) {
		t.Fatalf("the restart lost %s of %s: %v", block, session, blocks)
	}
	hooked := len(w.hookOutput("orchestrator"))
	w.relaunch(block, "orchestrator")

	if h := w.handleOf("orchestrator"); h != orch {
		t.Fatalf("the orchestrator came back as %s, not %s", h, orch)
	}
	after := w.leaseOf(orch)
	if after == nil || after["session_id"] != session || after["block_id"] != block || asInt(after["epoch"]) != asInt(before["epoch"])+1 {
		t.Fatalf("lease before the restart %v, after %v", before, after)
	}
	if fs, gen := w.fenceOf(orch); fs != session || gen != 0 {
		t.Errorf("fence after a restart in the same session: %s gen %d", fs, gen)
	}
	if n := w.unread(orch); n != 1 {
		t.Errorf("unread after the restart: %d", n)
	}
	if told := w.hookOutput("orchestrator")[hooked:]; !strings.Contains(told, "1 unread message(s)") {
		t.Errorf("the relaunched orchestrator was not told of its mail:\n%s", told)
	}
	if now, err := os.ReadFile(lifecycles[0]); err != nil || string(now) != string(lifecycle) {
		t.Errorf("the Epic's lifecycle changed:\n%s\nthen\n%s (%v)", lifecycle, now, err)
	}
	// The dead holder's departure retired its waker before the relaunch took
	// the handle back (retire on release), so a new waker rings for it now.
	wakerAfter := w.wakerPID(orch)
	t.Logf("waker of %s: pid %d before the restart, %d after", orch, wakerBefore, wakerAfter)
	if wakerAfter <= 0 {
		t.Fatalf("no waker after the relaunch\n%s", w.dump())
	}
	// Only the new mail on top of the kept one rings for two.
	w.send(peer, orch, "after the restart")
	w.until("a doorbell for both messages after the restart", 20*time.Second, func() bool {
		return strings.Contains(w.got("orchestrator"), "AMQ doorbell: 2 unread message(s) for "+orch)
	})
}

// A server that starts empty (--restore=false) loses every session: the
// conversation started again in a rebuilt session takes its handle over in a
// new session and block, under the next lease epoch, and its fence moves to
// the new session with the next generation.
func TestRexRebuildTakesTheHandleOverBehindANewFence(t *testing.T) {
	w := newWorld(t)
	w.newTab("solo", "implementer")
	w.ready("solo")
	h := w.handleOf("solo")
	before := w.leaseOf(h)
	fenceBefore, genBefore := w.fenceOf(h)
	if fenceBefore != before["session_id"] {
		t.Fatalf("fence %s, lease %v", fenceBefore, before)
	}

	w.restartServer(false)
	if out := w.rex("ls", "--json"); strings.Contains(out, "session:") {
		t.Fatalf("a server started without restore kept sessions:\n%s", out)
	}
	_ = os.Remove(filepath.Join(w.dir, "solo.ready"))
	rebuilt := w.newTab("solo", "implementer")
	w.ready("solo")

	if got := w.handleOf("solo"); got != h {
		t.Fatalf("the rebuilt conversation is %s, not %s", got, h)
	}
	after := w.leaseOf(h)
	if after == nil || after["session_id"] != rebuilt.session || after["block_id"] != rebuilt.first ||
		after["session_id"] == before["session_id"] || after["block_id"] == before["block_id"] ||
		asInt(after["epoch"]) != asInt(before["epoch"])+1 {
		t.Fatalf("lease before the rebuild %v, after %v (rebuilt in %s %s)", before, after, rebuilt.session, rebuilt.first)
	}
	if fs, gen := w.fenceOf(h); fs != rebuilt.session || gen != genBefore+1 {
		t.Fatalf("fence after the rebuild: %s gen %d, want %s gen %d", fs, gen, rebuilt.session, genBefore+1)
	}
	t.Logf("waker of %s after the rebuild: pid %d", h, w.wakerPID(h))
	w.send("solo", h, "after the rebuild") // a name, not a handle: amq refuses a self-send
	w.until("a doorbell in the rebuilt block", 20*time.Second, func() bool {
		return strings.Contains(w.got("solo"), "AMQ doorbell: 1 unread message(s) for "+h)
	})
}
