//go:build !darwin && !linux

package app

import (
	"fmt"
	"runtime"
)

func processAncestors() ([]int, error) {
	return nil, fmt.Errorf("reading process ancestry is not supported on %s", runtime.GOOS)
}
