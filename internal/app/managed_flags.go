package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

func newMailFlags(name string, stdout io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintf(stdout, "Usage: rex-amq-adapter %s [options]\n", name)
		fs.SetOutput(stdout)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	return fs
}

func parseMailFlags(fs *flag.FlagSet, args []string) (help bool, err error) {
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return true, nil
	} else if err != nil {
		reason := strings.Replace(err.Error(), "flag provided but not defined: -", "unknown flag --", 1)
		return false, fmt.Errorf("%w: %s: %s", ErrUsage, fs.Name(), reason)
	}
	if fs.NArg() != 0 {
		return false, fmt.Errorf("%w: %s: unexpected arguments %q", ErrUsage, fs.Name(), fs.Args())
	}
	return false, nil
}

// mailFlagArgs serializes only the parsed payload fields the handler names.
// Caller argv never crosses the authorization boundary into the child parser.
func mailFlagArgs(fs *flag.FlagSet, names ...string) []string {
	var args []string
	fs.Visit(func(f *flag.Flag) {
		if slices.Contains(names, f.Name) {
			args = append(args, "--"+f.Name+"="+f.Value.String())
		}
	})
	return args
}

func mailFlagPresent(fs *flag.FlagSet, name string) bool {
	present := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			present = true
		}
	})
	return present
}

func terminalInput(r io.Reader) bool {
	fd, ok := r.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(fd.Fd()), unix.TIOCGWINSZ)
	return err == nil
}
