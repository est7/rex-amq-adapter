package app

import (
	"fmt"
	"strconv"
	"strings"
)

// procStat is what the adapter reads from a Linux /proc/<pid>/stat line.
type procStat struct {
	Name string
	PPID int
	// StartTime is field 22, starttime: clock ticks after boot when the
	// process started.
	StartTime int64
}

// parseProcStat reads "pid (comm) state ppid … starttime …" (proc(5)). comm
// may itself hold spaces and parentheses, so fields are counted after the
// last ')': state is field 3, ppid 4, starttime 22. It is a pure function so
// that every platform's tests cover the Linux reader.
func parseProcStat(s string) (procStat, error) {
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return procStat{}, fmt.Errorf("no (comm) in %q", s)
	}
	fields := strings.Fields(s[end+1:])
	const state, ppid, starttime = 3, 4, 22
	if len(fields) <= starttime-state {
		return procStat{}, fmt.Errorf("%d fields after comm, want %d", len(fields), starttime-state+1)
	}
	p, err := strconv.Atoi(fields[ppid-state])
	if err != nil {
		return procStat{}, fmt.Errorf("ppid: %w", err)
	}
	st, err := strconv.ParseInt(fields[starttime-state], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("starttime: %w", err)
	}
	return procStat{Name: s[open+1 : end], PPID: p, StartTime: st}, nil
}
