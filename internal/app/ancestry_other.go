//go:build !darwin && !linux

package app

import (
	"fmt"
	"runtime"
)

func processAncestors() ([]proc, error) { return processAncestorsOf(0) }

func processAncestorsOf(int) ([]proc, error) {
	return nil, fmt.Errorf("reading process ancestry is not supported on %s", runtime.GOOS)
}

func processStartTime(int) (int64, error) {
	return 0, fmt.Errorf("reading a process's start time is not supported on %s", runtime.GOOS)
}
