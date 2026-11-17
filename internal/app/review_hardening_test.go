package app

import (
	"context"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An adoption that died between PutIdentity and AcquireLease leaves an
// identity that names the session and was never placed. The next hook
// finishes that adoption, and the agent is told who it is: it was never told.
func TestAdoptFinishesANeverPlacedIdentityWithItsNotice(t *testing.T) {
	e := realAmqEnv(t, &fakeTerm{state: "idle"})
	if err := e.Store.PutIdentity(store.Identity{Handle: "codex-01", Kind: core.Codex, Role: "reviewer", FirstAdoptedAt: e.Now(), Sessions: []string{"s1"}}); err != nil {
		t.Fatal(err)
	}
	r, err := e.Hook(context.Background(), EventPrompt, core.Codex, Location{Block: block}, HookInput{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if l := leaseOf(t, e, "codex-01"); l.BlockID != block || l.Adopting {
		t.Fatalf("the never-placed identity was not adopted: %+v", l)
	}
	if want := core.SessionNotice("codex-01", "reviewer", e.SelfBin, 0); !strings.Contains(r.Notice, want) {
		t.Fatalf("finishing a never-placed adoption told the agent nothing of who it is; notice %q, want it to hold %q", r.Notice, want)
	}
}
