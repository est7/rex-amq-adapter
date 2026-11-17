package app

import (
	"fmt"
	"os"
)

// processAncestors is this process and its ancestors, nearest first, read
// from /proc so a hook's fast path starts no subprocess.
func processAncestors() ([]proc, error) {
	var procs []proc
	for pid := os.Getpid(); pid > 1 && len(procs) < maxAncestors; {
		st, err := readProcStat(pid)
		if err != nil {
			return nil, err
		}
		procs = append(procs, proc{PID: pid, Name: st.Name})
		pid = st.PPID
	}
	return procs, nil
}

// processStartTime is when pid started, in clock ticks after boot.
func processStartTime(pid int) (int64, error) {
	st, err := readProcStat(pid)
	return st.StartTime, err
}

func readProcStat(pid int) (procStat, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStat{}, err
	}
	st, err := parseProcStat(string(b))
	if err != nil {
		return procStat{}, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	}
	return st, nil
}
