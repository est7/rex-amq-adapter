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
	if _, ok, err := s.EpicPolicy("session:e"); ok || err != nil {
		t.Fatalf("no policy yet: ok=%v err=%v", ok, err)
	}
	if err := s.PutEpicPolicy(EpicPolicy{Unattended: true}); err == nil {
		t.Error("a policy without its session was recorded")
	}
	want := EpicPolicy{SessionID: "session:e", Server: "unix:///s.sock", Unattended: true, CreatedBy: "claude-01", CreatedAt: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)}
	if err := s.PutEpicPolicy(want); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.EpicPolicy("session:e"); err != nil || !ok || got != want {
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
	if _, ok, _ := s.EpicPolicy("session:f"); ok {
		t.Error("another session took the policy")
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.EpicPolicy("session:e"); err == nil || ok {
		t.Errorf("a corrupt policy read as ok=%v err=%v", ok, err)
	}
}
