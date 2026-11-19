package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Materialize writes into a directory the user names, which an agent may be
// able to change while it runs: every directory is reached from a
// descriptor already held (pinDir), opened without following a link
// (O_NOFOLLOW), and every file under it is read, written, renamed, and
// removed relative to those descriptors (the *at calls), never by its path
// again. A directory renamed or swapped for a link after it was opened
// changes nothing: the descriptor still names the directory that was
// checked.

// maxLinkHops bounds the root-owned links pinDir follows (as SYMLOOP_MAX).
const maxLinkHops = 32

// pinned is --into, held by descriptor: dir when it exists, else parent,
// the directory that would hold it, and name, its last component. Close
// releases whichever it holds.
type pinned struct {
	dir    *os.File
	parent *os.File
	name   string
}

func (p pinned) Close() {
	for _, f := range []*os.File{p.dir, p.parent} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// pinDir opens the absolute, clean path into one component at a time from
// /, each with O_DIRECTORY|O_NOFOLLOW relative to the one before. A
// symbolic link on the way is followed only when root owns it (the
// system's, such as /var and /tmp on macOS, which no agent could have
// planted); any other is invalid_input. A missing last component pins its
// parent instead; a missing directory above it is an error.
func pinDir(into string) (pinned, error) {
	pending := components(into)
	cur, err := openDirAt(unix.AT_FDCWD, "/")
	if err != nil {
		return pinned{}, err
	}
	at, hops := "/", 0
	for len(pending) > 0 {
		c := pending[0]
		next, err := openDirAt(int(cur.Fd()), c)
		if err == nil {
			_ = cur.Close()
			cur, at, pending = next, filepath.Join(at, c), pending[1:]
			continue
		}
		var st unix.Stat_t
		serr := unix.Fstatat(int(cur.Fd()), c, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(serr, unix.ENOENT) && len(pending) == 1:
			return pinned{parent: cur, name: c}, nil
		case serr != nil:
			_ = cur.Close()
			return pinned{}, fmt.Errorf("--into %s: %s: %w", into, filepath.Join(at, c), serr)
		case uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK:
			link := filepath.Join(at, c)
			if st.Uid != 0 {
				_ = cur.Close()
				return pinned{}, fault.New(fault.InvalidInput, "--into %s goes through the symbolic link %s; name the real directory", into, link)
			}
			target, err := readlinkAt(int(cur.Fd()), c)
			_ = cur.Close()
			if err != nil {
				return pinned{}, fmt.Errorf("--into %s: read the link %s: %w", into, link, err)
			}
			if hops++; hops > maxLinkHops {
				return pinned{}, fault.New(fault.InvalidInput, "--into %s goes through more than %d symbolic links", into, maxLinkHops)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(at, target)
			}
			// The root-owned link is trusted: walk its target, and what
			// followed it, again from /.
			pending = components(filepath.Join(append([]string{target}, pending[1:]...)...))
			if cur, err = openDirAt(unix.AT_FDCWD, "/"); err != nil {
				return pinned{}, err
			}
			at = "/"
		case uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR:
			_ = cur.Close()
			return pinned{}, fault.New(fault.InvalidInput, "--into %s is not a directory (%s is not one)", into, filepath.Join(at, c))
		default:
			_ = cur.Close()
			return pinned{}, fmt.Errorf("--into %s: open %s: %w", into, filepath.Join(at, c), err)
		}
	}
	return pinned{dir: cur}, nil
}

// components are the names of the clean, absolute path p below /.
func components(p string) []string {
	var out []string
	for _, c := range strings.Split(filepath.Clean(p), "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// openDirAt opens the directory name under dirfd, never through a link.
func openDirAt(dirfd int, name string) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readlinkAt(dirfd int, name string) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(dirfd, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// splitName is an artifact name's directory ("" for none) and file name.
func splitName(name string) (dir, base string) {
	dir, base, ok := strings.Cut(name, "/")
	if !ok {
		return "", name
	}
	return dir, base
}

// readAt reads the artifact name under dir, never through a link: a link
// or anything but a directory at its directory, or anything but a regular
// file at it, is invalid_input. A file that is not there is (nil, false).
func readAt(dir *os.File, name string) ([]byte, bool, error) {
	parent, base := dir, name
	if sub, b := splitName(name); sub != "" {
		var st unix.Stat_t
		err := unix.Fstatat(int(dir.Fd()), sub, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, false, nil
		case err != nil:
			return nil, false, fault.New(fault.InvalidInput, "%s: %v", sub, err)
		case uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR:
			return nil, false, fault.New(fault.InvalidInput, "%s in the target directory is not a directory (%s); nothing was written", sub, fileType(st))
		}
		d, err := openDirAt(int(dir.Fd()), sub)
		if err != nil {
			return nil, false, fault.New(fault.InvalidInput, "%s in the target directory: %v; nothing was written", sub, err)
		}
		defer d.Close()
		parent, base = d, b
	}
	var st unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, false, nil
	case err != nil:
		return nil, false, fault.New(fault.InvalidInput, "%s: %v", name, err)
	case uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG:
		return nil, false, fault.New(fault.InvalidInput, "%s in the target directory is not a regular file (%s); nothing was written", name, fileType(st))
	}
	fd, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, fault.New(fault.InvalidInput, "%s in the target directory: %v; nothing was written", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false, fault.New(fault.InvalidInput, "%s in the target directory is not a regular file; nothing was written", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, store.ArtifactMaxBytes+1))
	return b, true, err
}

// writeAt replaces the artifact name under dir with data, atomically, 0644:
// a temporary file beside it, synced, renamed over it within the same
// directory descriptor.
func writeAt(dir *os.File, name string, data []byte) error {
	parent, base := dir, name
	if sub, b := splitName(name); sub != "" {
		if err := unix.Mkdirat(int(dir.Fd()), sub, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("make %s: %w", sub, err)
		}
		d, err := openDirAt(int(dir.Fd()), sub)
		if err != nil {
			return fmt.Errorf("open %s: %w", sub, err)
		}
		defer d.Close()
		parent, base = d, b
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	tmp := ".tmp-" + token + "-" + base
	pfd := int(parent.Fd())
	fd, err := unix.Openat(pfd, tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		// The umask may have narrowed it; through the descriptor, no
		// link at the name can redirect it.
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(pfd, tmp, pfd, base)
	}
	if err != nil {
		_ = unix.Unlinkat(pfd, tmp, 0)
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// verifyAt checks every artifact under dir against entries.
func verifyAt(dir *os.File, entries []store.ArtifactEntry) error {
	for _, a := range entries {
		b, ok, err := readAt(dir, a.Name)
		if err != nil {
			return err
		}
		if !ok || len(b) != a.Bytes || store.RawSHA256(b) != a.SHA256 {
			return fmt.Errorf("artifact %s was not written as read", a.Name)
		}
	}
	return nil
}

// removeAllAt removes name under dirfd and everything under it, never
// through a link: a link is removed, not followed.
func removeAllAt(dirfd int, name string) error {
	err := unix.Unlinkat(dirfd, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	d, oerr := openDirAt(dirfd, name)
	if oerr != nil {
		return err
	}
	entries, rerr := d.ReadDir(-1)
	for _, en := range entries {
		if err := removeAllAt(int(d.Fd()), en.Name()); err != nil && rerr == nil {
			rerr = err
		}
	}
	_ = d.Close()
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return rerr
}

func fileType(st unix.Stat_t) string {
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFLNK:
		return "a symbolic link"
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFREG:
		return "a regular file"
	case unix.S_IFIFO:
		return "a FIFO"
	case unix.S_IFSOCK:
		return "a socket"
	}
	return "a device"
}
