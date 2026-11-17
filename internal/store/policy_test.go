package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An Epic's policy reads back as written, from its session's private
// extension directory (0700, the file 0600); a session no epic opened has
// none, and a corrupt policy is an error, not an attended session.
func TestEpicPolicyStore(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if _, ok, err := s.SessionPolicy("session:e"); ok || err != nil {
		t.Fatalf("no policy yet: ok=%v err=%v", ok, err)
	}
	if err := s.PutSessionPolicy(SessionPolicy{Kind: PolicyEpic, Unattended: true}); err == nil {
		t.Error("a policy without its session was recorded")
	}
	want := SessionPolicy{Kind: PolicyEpic, SessionID: "session:e", Server: "unix:///s.sock", Unattended: true, CreatedBy: "claude-01", CreatedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}
	if err := s.PutSessionPolicy(want); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.SessionPolicy("session:e"); err != nil || !ok || got != want {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	path := filepath.Join(s.P.Root(), "extensions", "rex-amq-adapter", "sessions", "session_e", "policy.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("policy file %s: %v %v, want mode 0600", path, info, err)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("policy directory: %v %v, want mode 0700", dir, err)
	}
	if _, ok, _ := s.SessionPolicy("session:f"); ok {
		t.Error("another session took the policy")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.SessionPolicy("session:e"); err == nil || ok {
		t.Errorf("a corrupt policy read as ok=%v err=%v", ok, err)
	}
}

// A policy written before policies had a kind (only epic wrote one) reads
// as an Epic's, unattended as it was; a Hub's reads as a Hub's, and an
// unknown kind is an error.
func TestSessionPolicyReadsALegacyEpicPolicy(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	path := s.P.policyPath("session:old")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"session_id":"session:old","server":"unix:///s.sock","unattended":true,"created_by":"claude-01","created_at":"2026-11-15T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.SessionPolicy("session:old")
	if err != nil || !ok || !p.Epic() || p.Hub() || !p.Unattended || p.Kind != "" {
		t.Fatalf("legacy policy %+v %v %v", p, ok, err)
	}
	hub := SessionPolicy{Kind: PolicyHub, SessionID: "session:h", HubKey: "unix:///s.sock|/me"}
	if err := s.PutSessionPolicy(hub); err != nil {
		t.Fatal(err)
	}
	if p, ok, _ := s.SessionPolicy("session:h"); !ok || !p.Hub() || p.Epic() || p.HubKey != hub.HubKey {
		t.Fatalf("hub policy %+v", p)
	}
	if err := os.WriteFile(path, []byte(`{"kind":"other","session_id":"session:old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.SessionPolicy("session:old"); err == nil || ok {
		t.Errorf("an unknown kind read as ok=%v err=%v", ok, err)
	}
}
