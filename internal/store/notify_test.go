package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A notification is found again by its event id, in a private file named by
// the id's hash: an id a producer chose never becomes a path.
func TestNotifyRecordRoundTrips(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	id := "../../evil/event 1"
	if _, ok, err := s.Notification(id); ok || err != nil {
		t.Fatalf("before any: ok=%v err=%v", ok, err)
	}
	n := Notification{EventID: id, Ref: "inbox:42", Hub: "/hub", Personal: "claude-01", Handle: "claude-01", SessionID: "session:p", MessageID: "m1", At: time.Unix(100, 0).UTC()}
	if err := s.PutNotification(n); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Notification(id)
	if err != nil || !ok || got != n {
		t.Fatalf("got %+v ok=%v err=%v, want %+v", got, ok, err, n)
	}
	if _, ok, _ := s.Notification(id + "x"); ok {
		t.Fatal("another event id found the record")
	}
	files, _ := filepath.Glob(filepath.Join(s.P.Extensions(), "notifications", "*.json"))
	if len(files) != 1 || len(filepath.Base(files[0])) != 64+len(".json") {
		t.Fatalf("files %v", files)
	}
	for path, want := range map[string]os.FileMode{files[0]: 0o600, filepath.Dir(files[0]): 0o700} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, fi.Mode().Perm(), err, want)
		}
	}
	if err := s.PutNotification(Notification{EventID: "e"}); err == nil {
		t.Error("a notification without a message id was recorded")
	}
}

// A record that cannot be read is an error, never "not sent": the event
// would be sent twice.
func TestNotifyUnreadableRecordIsAnError(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	if err := os.MkdirAll(s.P.notifications(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.notificationPath("e1"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Notification("e1"); ok || err == nil {
		t.Fatalf("ok=%v err=%v, want an error", ok, err)
	}
}

// LockNotify excludes a second holder until the first lets go.
func TestNotifyLockExcludes(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	unlock, err := s.LockNotify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if u, err := s.LockNotify(ctx); err == nil {
		u()
		t.Fatal("a second holder took the lock")
	}
	unlock()
	u, err := s.LockNotify(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u()
}
