package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func agentEnv(t *testing.T, now time.Time) Env {
	t.Helper()
	paths := store.Paths{Home: t.TempDir()}
	return Env{Paths: paths, Store: store.Store{P: paths}, Now: func() time.Time { return now }}
}

// A block can hold one live agent and the departed handles of earlier ones;
// agentAt returns the live one, else the last to depart.
func TestAgentAtPrefersTheLiveAgentOfABlock(t *testing.T) {
	early := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	e := agentEnv(t, late)
	plant(t, e, seed{Handle: "codex-01", BlockID: "block:a", DepartedAt: &early})
	plant(t, e, seed{Handle: "codex-02", BlockID: "block:a", DepartedAt: &late})
	plant(t, e, seed{Handle: "codex-03", BlockID: "block:b"})
	if got, ok, _ := e.agentAt("block:a"); !ok || got.Handle != "codex-02" || got.Live() {
		t.Fatalf("departed only: got %+v, want the last to depart", got)
	}
	plant(t, e, seed{Handle: "claude-01", BlockID: "block:a"})
	if got, ok, _ := e.agentAt("block:a"); !ok || got.Handle != "claude-01" || !got.Live() {
		t.Fatalf("with a live agent: got %+v", got)
	}
	if _, ok, _ := e.liveAt("block:none"); ok {
		t.Fatal("a block nobody ran in has a live agent")
	}
}

// An adoption that died between writing the identity and acquiring its lease
// leaves a handle placed nowhere: it reads as not running, keeps its name for
// the reservation, and collection forgets it after that.
func TestCollectForgetsANeverPlacedIdentityAfterItsReservation(t *testing.T) {
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := first.Add(ReserveFor - time.Minute)
	e := agentEnv(t, now)
	e.Now = func() time.Time { return now }
	if err := e.Store.PutIdentity(store.Identity{Handle: "codex-01", Kind: core.Codex, FirstAdoptedAt: first}); err != nil {
		t.Fatal(err)
	}
	a, ok, err := e.agentOf("codex-01")
	if err != nil || !ok || a.Live() || a.departure() != nil || a.BlockID != "" {
		t.Fatalf("never placed: %+v ok=%v err=%v", a, ok, err)
	}
	obs, err := e.observeAgents(view{}, []agent{a}, nil)
	if err != nil || len(obs) != 1 || !obs[0].Adopting || obs[0].DepartedAt != nil {
		t.Fatalf("a never-placed handle projects as %+v (%v), want an unfinished adoption", obs, err)
	}
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Store.IdentityOf("codex-01"); !ok {
		t.Fatal("collected inside its reservation")
	}
	now = first.Add(ReserveFor + time.Minute)
	if err := e.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.Store.IdentityOf("codex-01"); ok {
		t.Fatal("kept past its reservation")
	}
}

// A lease whose identity is gone names nobody: listing agents fails rather
// than dropping a running agent's lease from view.
func TestAgentsRefuseALeaseWithoutAnIdentity(t *testing.T) {
	e := agentEnv(t, time.Now())
	plant(t, e, seed{Handle: "codex-01", BlockID: "block:a"})
	if err := os.Remove(filepath.Join(e.Paths.Home, "identities", "codex-01.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.agents(); err == nil {
		t.Fatal("a lease without an identity was listed")
	}
	if _, _, err := e.agentAt("block:a"); err == nil {
		t.Fatal("a lease without an identity was read as a block's agent")
	}
}
