package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processAncestors is this process and its ancestors, nearest first, read
// from /proc so a hook's fast path starts no subprocess.
func processAncestors() ([]int, error) {
	var pids []int
	for pid := os.Getpid(); pid > 1 && len(pids) < maxAncestors; {
		pids = append(pids, pid)
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return nil, err
		}
		// "pid (comm) state ppid …"; comm may itself hold spaces and parentheses.
		s := string(b)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) < 2 {
			return nil, fmt.Errorf("unreadable /proc/%d/stat", pid)
		}
		if pid, err = strconv.Atoi(fields[1]); err != nil {
			return nil, fmt.Errorf("/proc/%d/stat: %w", pids[len(pids)-1], err)
		}
	}
	return pids, nil
}
