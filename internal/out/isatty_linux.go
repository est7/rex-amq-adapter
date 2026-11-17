//go:build linux

package out

import "golang.org/x/sys/unix"

// isTerminal is a real isatty: the terminal ioctl succeeds only on a tty.
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}
