package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// processAncestors is this process and its ancestors, nearest first. It reads
// the kernel's process table with sysctl, so a hook's fast path starts no
// subprocess (about 80 µs for five levels).
func processAncestors() ([]int, error) {
	var pids []int
	for pid := os.Getpid(); pid > 1 && len(pids) < maxAncestors; {
		pids = append(pids, pid)
		k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return nil, err
		}
		pid = int(k.Eproc.Ppid)
	}
	return pids, nil
}
