package store

import (
	"testing"
	"time"
)

// A lifecycle lists each hand-back Seq once, whichever entry it is: one
// noted again (an earlier report backfilled, or a report whose store failed
// noted over again) replaces its entry, wherever it is.
func TestNoteHandBackKeepsOneEntryPerSeq(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	var l EpicLifecycle
	l.NoteHandBack(HandBack{Seq: 1, Digest: "sha256:a", At: at})
	l.NoteHandBack(HandBack{Seq: 2, Digest: "sha256:b", At: at})
	l.NoteHandBack(HandBack{Seq: 1, Digest: "sha256:a", At: at})
	l.NoteHandBack(HandBack{Seq: 2, Digest: "sha256:c", At: at})
	if len(l.HandBacks) != 2 || l.HandBacks[0].Digest != "sha256:a" || l.HandBacks[1].Seq != 2 || l.HandBacks[1].Digest != "sha256:c" || l.State != EpicHandBack {
		t.Fatalf("hand-backs %+v, state %q", l.HandBacks, l.State)
	}
}
