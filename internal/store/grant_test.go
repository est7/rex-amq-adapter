package store

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A grant is notarized once, read-only: a second PutGrant returns the kept
// record untouched, a missing grant.md is written from it, and a goal is
// never replaced by other text.
func TestGrantIsWrittenOnceReadOnly(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	at := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	g := Grant{Asked: "fix x, y, z.\n\nFull stop with council 9.5", RelayedBy: "claude-01", RelayedAt: at, SessionID: "session:e", Contract: s.P.GoalPath("session:e"), ContractSHA256: ContractDigest([]byte("GOAL: x\n"))}
	if err := s.PutGoal("session:e", "GOAL: x\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutGoal("session:e", "GOAL: x\n"); err != nil {
		t.Fatalf("the same goal again: %v", err)
	}
	if err := s.PutGoal("session:e", "GOAL: y\n"); err == nil {
		t.Fatal("a goal was replaced by other text")
	}
	kept, err := s.PutGrant(g)
	if err != nil || kept != g {
		t.Fatalf("PutGrant = %+v, %v", kept, err)
	}
	later := g
	later.Asked, later.RelayedAt = "something else", at.Add(time.Hour)
	if kept, err := s.PutGrant(later); err != nil || kept != g {
		t.Fatalf("a retry rewrote the grant: %+v, %v", kept, err)
	}
	for _, p := range []string{s.P.GoalPath("session:e"), s.P.GrantPath("session:e"), s.P.grantJSONPath("session:e")} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o444 {
			t.Errorf("%s: %v %v", p, info.Mode(), err)
		}
	}
	md, _ := os.ReadFile(s.P.GrantPath("session:e"))
	for _, want := range []string{"relayed_by: claude-01", "relayed_at: 2026-11-16T08:00:00Z", "> fix x, y, z.\n>\n> Full stop with council 9.5\n", "contract: " + g.Contract, "contract_sha256: " + g.ContractSHA256} {
		if !strings.Contains(string(md), want) {
			t.Errorf("grant.md lacks %q:\n%s", want, md)
		}
	}
	if err := os.Remove(s.P.GrantPath("session:e")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutGrant(later); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(s.P.GrantPath("session:e")); string(again) != string(md) {
		t.Errorf("grant.md rewritten from the retry, not the kept record:\n%s", again)
	}
	if _, err := s.PutGrant(Grant{SessionID: "session:f", RelayedBy: "claude-01"}); err == nil {
		t.Error("a grant with neither words nor a contract was kept")
	}
}
