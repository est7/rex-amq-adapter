package app

import (
	"os"

	"golang.org/x/sys/unix"
)

// processAncestors is this process and its ancestors, nearest first. It reads
// the kernel's process table with sysctl, so a hook's fast path starts no
// subprocess (about 80 µs for five levels).
func processAncestors() ([]proc, error) {
	var procs []proc
	for pid := os.Getpid(); pid > 1 && len(procs) < maxAncestors; {
		k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return nil, err
		}
		procs = append(procs, proc{PID: pid, Name: unix.ByteSliceToString(k.Proc.P_comm[:])})
		pid = int(k.Eproc.Ppid)
	}
	return procs, nil
}
