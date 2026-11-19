//go:build !darwin && !freebsd && !netbsd && !openbsd && !linux

package out

// isTerminal is false where the terminal ioctl is not known: such a
// platform always gets machine output unless --text asks for text.
func isTerminal(uintptr) bool { return false }
