package store

import (
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func TestTurnRecordRoundTripAndForgetIdentityCleanup(t *testing.T) {
	s := testStore(t)
	if _, ok, err := s.Turn("agy-42"); err != nil || ok {
		t.Fatalf("missing turn record: ok=%v err=%v", ok, err)
	}
	want := TurnRecord{
		Seq:         3,
		Phase:       core.TurnWorking,
		At:          t0,
		Incarnation: Incarnation{PID: 4242, StartTime: 987654321},
		SessionID:   "conv-42",
	}
	if err := s.PutTurn("agy-42", want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Turn("agy-42")
	if err != nil || !ok || got.Seq != want.Seq || got.Phase != want.Phase || !got.At.Equal(want.At) || got.Incarnation != want.Incarnation || got.SessionID != want.SessionID {
		t.Fatalf("round trip: got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}

	// Reject invalid phase on write.
	if err := s.PutTurn("agy-42", TurnRecord{Phase: "bogus", At: t0}); err == nil {
		t.Fatal("invalid turn phase accepted")
	}

	// ForgetIdentity cleans up turn/<handle>.json along with identity state.
	mustIdentity(t, s, Identity{Handle: "agy-42", Kind: core.Agy})
	if err := s.ForgetIdentity("agy-42"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Turn("agy-42"); err != nil || ok {
		t.Fatalf("ForgetIdentity left turn record behind: ok=%v err=%v", ok, err)
	}
}

func TestTurnRecordClearMissingIsNoOp(t *testing.T) {
	s := testStore(t)
	if err := s.ClearTurn("codex-99"); err != nil {
		t.Fatalf("ClearTurn missing: %v", err)
	}
	if err := s.PutTurn("codex-99", TurnRecord{Seq: 1, Phase: core.TurnIdle, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearTurn("codex-99"); err != nil {
		t.Fatalf("ClearTurn existing: %v", err)
	}
	if _, ok, err := s.Turn("codex-99"); err != nil || ok {
		t.Fatalf("after ClearTurn: ok=%v err=%v", ok, err)
	}
}
