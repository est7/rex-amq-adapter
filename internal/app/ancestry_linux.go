package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processAncestors is this process and its ancestors, nearest first, read
// from /proc so a hook's fast path starts no subprocess.
func processAncestors() ([]proc, error) {
	var procs []proc
	for pid := os.Getpid(); pid > 1 && len(procs) < maxAncestors; {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil, err
		}
		// "pid (comm) state ppid …"; comm may itself hold spaces and parentheses.
		s := string(b)
		open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || end < open {
			return nil, fmt.Errorf("unreadable /proc/%d/stat", pid)
		}
		procs = append(procs, proc{PID: pid, Name: s[open+1 : end]})
		fields := strings.Fields(s[end+1:])
		if len(fields) < 2 {
			return nil, fmt.Errorf("unreadable /proc/%d/stat", pid)
		}
		if pid, err = strconv.Atoi(fields[1]); err != nil {
			return nil, fmt.Errorf("/proc/%d/stat: %w", procs[len(procs)-1].PID, err)
		}
	}
	return procs, nil
}
