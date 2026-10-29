package rex

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestVersionOfPicksTheNumericDirectory(t *testing.T) {
	newer := "/h/Library/Application Support/com.superlogical.rex/RexServer/Versions/1210/Rex Server.app/Contents/MacOS/rex"
	older := "/h/Library/Application Support/com.superlogical.rex/RexServer/Versions/987/Rex Server.app/Contents/MacOS/rex"
	if versionOf(newer) != 1210 || versionOf(older) != 987 {
		t.Fatalf("got %d, %d", versionOf(newer), versionOf(older))
	}
	if versionOf("/usr/bin/rex") != -1 {
		t.Fatal("unversioned path must sort last")
	}
}

// TestLiveClient runs against the Rex server of the terminal running the
// tests. Set REX_AMQ_LIVE=1 inside a Rex terminal.
func TestLiveClient(t *testing.T) {
	block := os.Getenv("REX_BLOCK")
	if os.Getenv("REX_AMQ_LIVE") != "1" || block == "" {
		t.Skip("set REX_AMQ_LIVE=1 inside a Rex terminal")
	}
	bin, err := ResolveBin()
	if err != nil {
		t.Fatal(err)
	}
	c := Client{Bin: bin, Server: os.Getenv("REX_SERVER")}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	srv, err := c.Whoami(ctx)
	if err != nil || srv.InstanceID == "" {
		t.Fatalf("whoami: %+v %v", srv, err)
	}
	b, err := c.InspectBlock(ctx, block)
	if err != nil || b.ID != block || b.SessionID == "" || b.WindowID == "" {
		t.Fatalf("inspect: %+v %v", b, err)
	}
	s, err := c.InspectSession(ctx, b.SessionID)
	if err != nil || s.ID != b.SessionID || s.Label == "" {
		t.Fatalf("inspect session: %+v %v", s, err)
	}
	inSession := false
	for _, w := range s.Windows {
		inSession = inSession || w.ID == b.WindowID
	}
	if !inSession {
		t.Fatalf("block's window %s not among the session's windows %+v", b.WindowID, s.Windows)
	}
	p, ok, err := c.Foreground(ctx, block)
	if err != nil || !ok || p.PID == 0 {
		t.Fatalf("foreground: %+v %v %v", p, ok, err)
	}
	if _, _, err := c.ProgramStatus(ctx, block); err != nil {
		t.Fatalf("program status: %v", err)
	}
	_, _, err = c.Foreground(ctx, "block:01a15999-0000-7000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing block: want ErrNotFound, got %v", err)
	}
	unreachable := Client{Bin: bin, Server: "unix:///nonexistent-rex-amq.sock"}
	if _, err := unreachable.Whoami(ctx); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("unreachable server: want ErrUnreachable, got %v", err)
	}
}
