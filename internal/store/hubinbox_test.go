package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Entries are numbered from 1 in the order they are appended; one whose
// dedupe key is there already appends nothing and returns the first.
func TestHubInboxAppendsOnceByDedupeKey(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	ctx, now := context.Background(), time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	hub := "/hub/a"
	first, added, err := s.AppendHubInbox(ctx, hub, HubInboxEntry{Epic: "session:e", Kind: InboxHandBack, HandBackSeq: 1, Digest: "d1", By: "claude-02", DedupeKey: "handback:session:e#1"}, now)
	if err != nil || !added || first.Seq != 1 || first.Schema != HubInboxSchema || !first.At.Equal(now) {
		t.Fatalf("first: %+v %v %v", first, added, err)
	}
	again, added, err := s.AppendHubInbox(ctx, hub, HubInboxEntry{Epic: "session:e", Kind: InboxHandBack, HandBackSeq: 1, Digest: "other", By: "x", DedupeKey: "handback:session:e#1"}, now.Add(time.Hour))
	if err != nil || added || again != first {
		t.Fatalf("again: %+v %v %v", again, added, err)
	}
	second, added, err := s.AppendHubInbox(ctx, hub, HubInboxEntry{Epic: "session:e", Kind: InboxNarrator, Body: "hand-back ready", By: "agy-01", DedupeKey: "narrator:1"}, now)
	if err != nil || !added || second.Seq != 2 {
		t.Fatalf("second: %+v %v %v", second, added, err)
	}
	if other, _, err := s.AppendHubInbox(ctx, "/hub/b", HubInboxEntry{Epic: "session:f", Kind: InboxNarrator, By: "agy-02", DedupeKey: "narrator:1"}, now); err != nil || other.Seq != 1 {
		t.Errorf("another Hub's inbox: %+v %v", other, err)
	}
	got, err := s.HubInbox(hub)
	if err != nil || len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("read: %+v %v", got, err)
	}
	if got, err := s.HubInbox("/hub/none"); err != nil || len(got) != 0 {
		t.Errorf("a Hub with no inbox: %+v %v", got, err)
	}
}

// A line a writer died writing is not read, and the next writer cuts it off
// before it appends, so no line is ever two entries run together.
func TestHubInboxSkipsAndRepairsATornLastLine(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	ctx, now := context.Background(), time.Now()
	hub := "/hub/a"
	if _, _, err := s.AppendHubInbox(ctx, hub, HubInboxEntry{Epic: "session:e", Kind: InboxNarrator, By: "agy-01", DedupeKey: "k1"}, now); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.P.HubInboxPath(hub), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema":1,"seq":2,"epic":"session:e","kind":"narr`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, err := s.HubInbox(hub); err != nil || len(got) != 1 {
		t.Fatalf("read past a torn line: %+v %v", got, err)
	}
	e, added, err := s.AppendHubInbox(ctx, hub, HubInboxEntry{Epic: "session:e", Kind: InboxNarrator, By: "agy-01", DedupeKey: "k2"}, now)
	if err != nil || !added || e.Seq != 2 {
		t.Fatalf("append after a torn line: %+v %v %v", e, added, err)
	}
	if got, err := s.HubInbox(hub); err != nil || len(got) != 2 || got[1].DedupeKey != "k2" {
		t.Fatalf("read after the repair: %+v %v", got, err)
	}
}

// Writers of one Hub take turns: concurrent appends get distinct,
// consecutive seqs, and a reader running beside them reads whole entries.
func TestHubInboxConcurrentAppends(t *testing.T) {
	s := Store{P: Paths{Home: t.TempDir()}}
	ctx, now := context.Background(), time.Now()
	hub := "/hub/a"
	const n = 40
	var wg sync.WaitGroup
	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				readErr <- nil
				return
			default:
			}
			if _, err := s.HubInbox(hub); err != nil {
				readErr <- err
				return
			}
		}
	}()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := s.AppendHubInbox(ctx, hub, HubInboxEntry{Epic: "session:e", Kind: InboxNarrator, By: "agy-01", DedupeKey: fmt.Sprint("k", i)}, now); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	if err := <-readErr; err != nil {
		t.Fatalf("a lock-free reader: %v", err)
	}
	got, err := s.HubInbox(hub)
	if err != nil || len(got) != n {
		t.Fatalf("read: %d entries, %v", len(got), err)
	}
	for i, e := range got {
		if e.Seq != int64(i+1) {
			t.Fatalf("entry %d has seq %d", i, e.Seq)
		}
	}
}
