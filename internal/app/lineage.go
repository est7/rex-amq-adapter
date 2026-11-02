package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// lineage is the session ids that make up in's conversation, its own first,
// then its ancestors, nearest first. Claude Code can move a conversation to a
// new session id without telling its hooks: the record keeps the old id, and
// the only proof that the new id continues it is the transcript, which starts
// with the old entries under their old id. An official fork rewrites those
// entries and so has no ancestors here; it is a new agent.
//
// The transcript format is Claude Code's own, so a lineage that cannot be read
// is reported and left out: the agent is named afresh, as without this check.
func lineage(recs []store.Record, kind core.Kind, in HookInput) []string {
	if in.SessionID == "" {
		return nil
	}
	sessions := []string{in.SessionID}
	if kind != core.Claude || sessionRecorded(recs, kind, in.SessionID) {
		return sessions
	}
	path, err := in.transcript()
	if err == nil && path != "" {
		var ancestors []string
		if ancestors, err = transcriptAncestors(path, in.SessionID); err == nil {
			return append(sessions, ancestors...)
		}
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the lineage of session %s: %v\n", in.SessionID, err)
	}
	return sessions
}

func sessionRecorded(recs []store.Record, kind core.Kind, session string) bool {
	return slices.ContainsFunc(recs, func(r store.Record) bool {
		return r.Kind == kind && r.AgentSessionID == session
	})
}

// transcriptAncestors returns the other session ids in the entries before
// session's own, nearest first. A session that has not written a transcript yet
// has none. A line that does not parse is skipped: an agent killed mid-append
// leaves a torn one, and the entries around it still prove the lineage.
func transcriptAncestors(path, session string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ancestors []string
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		var entry struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.SessionID != "" {
			if entry.SessionID == session {
				break
			}
			ancestors = append(slices.DeleteFunc(ancestors, func(s string) bool { return s == entry.SessionID }), entry.SessionID)
		}
		if err != nil {
			break
		}
	}
	slices.Reverse(ancestors)
	return ancestors, nil
}
