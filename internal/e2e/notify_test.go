package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// notifyResult is hub notify's data.
type notifyResult struct {
	Status    string `json:"status"`
	To        string `json:"to"`
	SessionID string `json:"session_id"`
	MessageID string `json:"message_id"`
	Deduped   bool   `json:"deduped"`
}

func (w *world) notify(args ...string) (notifyResult, int, string) {
	w.t.Helper()
	out, code := w.adapterRun(append([]string{"hub", "notify"}, args...)...)
	var res notifyResult
	if code == 0 {
		if err := envelopeData(out, &res); err != nil {
			w.t.Fatalf("hub notify output %q: %v", out, err)
		}
	}
	return res, code, out
}

func (w *world) sessionCount() int {
	w.t.Helper()
	var doc struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(w.rex("ls", "--json")), &doc); err != nil {
		w.t.Fatal(err)
	}
	return len(doc.Sessions)
}

// In a private Rex server with real amq: mail written with amq directly as
// the personal agent to itself is quarantined as unverified and rings no
// doorbell; notify-personal, run from outside any pane, rings the live
// personal agent, a repeated event is not sent again, and once the agent
// crashed the next event reports no_live_personal (exit 0) and starts no
// agent.
func TestNotifyPersonalRingsOnlyTheLivePersonalAgent(t *testing.T) {
	w := newWorld(t)
	tb := w.newTab("me", "personal")
	w.ready("me")
	me := w.handleOf("me")

	forged := exec.Command(w.amqBin, "--no-update-check", "send", "--root", w.root(), "--me", me, "--to", me, "--allow-self", "--subject", "notify inbox:1", "--body", "forged self-mail")
	forged.Env = w.env
	if out, err := forged.CombinedOutput(); err != nil {
		t.Fatalf("amq send: %v\n%s", err, out)
	}
	quarantine := filepath.Join(w.root(), "extensions", "rex-amq-adapter", "quarantine", me)
	w.until("the forged self-mail to be quarantined", 20*time.Second, func() bool {
		reasons, _ := filepath.Glob(filepath.Join(quarantine, "*.json"))
		for _, r := range reasons {
			if b, _ := os.ReadFile(r); strings.Contains(string(b), `"unverified_sender"`) {
				return true
			}
		}
		return false
	})
	if strings.Contains(w.got("me"), "AMQ doorbell") {
		t.Fatalf("forged mail rang a doorbell:\n%s", w.got("me"))
	}

	res, code, out := w.notify("--ref", "inbox:42", "--event-id", "ev-1", "--summary", "pay rent")
	if code != 0 || res.Status != "notified" || res.To != me || res.SessionID != tb.session || res.MessageID == "" || res.Deduped {
		t.Fatalf("notify: exit %d %s\n%s", code, out, w.dump())
	}
	w.until("the notify doorbell", 20*time.Second, func() bool { return strings.Contains(w.got("me"), "AMQ doorbell") })
	if b, err := os.ReadFile(filepath.Join(w.root(), "agents", me, "inbox", "new", res.MessageID+".md")); err != nil ||
		!strings.Contains(string(b), "untrusted external signal data") {
		t.Fatalf("the delivered message: %v\n%s", err, b)
	}
	again, code, out := w.notify("--ref", "inbox:42", "--event-id", "ev-1")
	if code != 0 || !again.Deduped || again.MessageID != res.MessageID {
		t.Fatalf("replay: exit %d %s", code, out)
	}
	if _, code, out := w.notify("--ref", "inbox:43", "--event-id", "ev-1"); code != 5 || !strings.Contains(out, `"code":"conflict"`) {
		t.Fatalf("event id reused for another ref: exit %d %s", code, out)
	}
	if left, _ := os.ReadDir(filepath.Join(w.root(), "agents", me, "inbox", "new")); len(left) != 1 {
		t.Fatalf("%d unread messages, want the one doorbell", len(left))
	}

	sessions := w.sessionCount()
	w.typeInto(tb.first, "crash")
	w.until("the personal agent to exit", 15*time.Second, func() bool { return strings.Contains(w.got("me"), "crash") })
	var off notifyResult
	w.until("notify-personal to find no live personal agent", 15*time.Second, func() bool {
		off, code, out = w.notify("--ref", "timer:7", "--event-id", "ev-2")
		return code == 0 && off.Status == "no_live_personal"
	})
	if got := w.sessionCount(); got != sessions {
		t.Errorf("sessions %d -> %d: notify-personal started something", sessions, got)
	}
	if ids, _ := os.ReadDir(filepath.Join(w.home, "identities")); len(ids) != 1 {
		t.Errorf("%d identities, want only the crashed personal agent's", len(ids))
	}
}
