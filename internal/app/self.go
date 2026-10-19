package app

import (
	"os"
	"path/filepath"
	"strings"
)

// SelfPath is the path hooks and wakers run this binary by. It is the
// symlink-free file, except under Homebrew: there the real file lives in a
// versioned Cellar directory that `brew upgrade` deletes, so the stable
// <prefix>/opt/<formula>/bin path is used instead. amq stores an
// --inject-via path resolved, so a waker started before an upgrade is
// replaced by the next supervision (its saved target no longer matches).
func SelfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return stablePath(real), nil
}

// stablePath maps <prefix>/Cellar/<formula>/<version>/bin/<file> to
// <prefix>/opt/<formula>/bin/<file> when that path leads to the same file.
func stablePath(real string) string {
	parts := strings.Split(filepath.ToSlash(real), "/")
	for i := len(parts) - 5; i >= 0; i-- {
		if parts[i] != "Cellar" || parts[i+3] != "bin" || i+5 != len(parts) {
			continue
		}
		prefix := filepath.FromSlash(strings.Join(parts[:i], "/"))
		opt := filepath.Join(prefix, "opt", parts[i+1], "bin", parts[i+4])
		if resolved, err := filepath.EvalSymlinks(opt); err == nil && resolved == real {
			return opt
		}
		return real
	}
	return real
}
