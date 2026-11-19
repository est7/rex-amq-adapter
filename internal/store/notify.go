package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// hub notify's dedup records live beside the gateway's state:
//
//   - notifications/<sha256(event id)>.json: the doorbell sent for an outside
//     producer's event (Notification), so a retried event is not sent again.
//   - notifications.lock: held while one event is looked up and sent.

func (p Paths) notifications() string { return filepath.Join(p.Extensions(), "notifications") }

// Notification is the doorbell hub notify sent for one event of an
// outside producer (an Inbox or Timer entry): the request (the ref, and the
// --hub and --personal it was narrowed by, empty when not given), and the
// personal agent, session, and message it reached.
type Notification struct {
	EventID   string      `json:"event_id"`
	Ref       string      `json:"ref"`
	Hub       string      `json:"hub,omitempty"`      // symlink-resolved
	Personal  core.Handle `json:"personal,omitempty"` // as asked, not the one reached
	Handle    core.Handle `json:"handle"`
	SessionID string      `json:"session_id"`
	MessageID string      `json:"message_id"`
	At        time.Time   `json:"at"`
}

// The file is named by a hash of the event id: the producer chooses the id,
// so it never becomes a path.
func (s Store) notificationPath(eventID string) string {
	sum := sha256.Sum256([]byte(eventID))
	return filepath.Join(s.P.notifications(), hex.EncodeToString(sum[:])+".json")
}

// LockNotify serialises hub notify, so two runs for one event never both
// send.
func (s Store) LockNotify(ctx context.Context) (func(), error) {
	return fsutil.Flock(ctx, filepath.Join(s.P.Extensions(), "notifications.lock"))
}

// Notification finds the doorbell sent for eventID; ok is false when none
// was. An unreadable record is an error: treating it as absent would send the
// event twice.
func (s Store) Notification(eventID string) (n Notification, ok bool, err error) {
	path := s.notificationPath(eventID)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return n, false, nil
	}
	if err != nil {
		return n, false, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, false, fmt.Errorf("decode %s: %w", path, err)
	}
	if n.EventID != eventID {
		return Notification{}, false, fmt.Errorf("%s records event %q, not %q", path, n.EventID, eventID)
	}
	return n, true, nil
}

// PutNotification records n, replacing any record of its event.
func (s Store) PutNotification(n Notification) error {
	if n.EventID == "" || n.MessageID == "" {
		return errors.New("a notification needs its event id and message id")
	}
	return writeJSON(s.notificationPath(n.EventID), n)
}
