package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const handBackUsage = "usage: rex-amq-adapter hand-back TEXT|@FILE|-   (the whole final report; - reads stdin; `--` before a TEXT that starts with a dash)"

// handBackReadMax is how much of stdin a hand-back reads: past the 256 KiB
// the adapter takes, so an oversized report is refused, not cut.
const handBackReadMax = 256<<10 + 1

// handBackBody is the report a hand-back's one argument names: the text
// itself, @FILE for a file's contents, or - for stdin.
func handBackBody(args []string, stdin io.Reader) (string, error) {
	if len(args) == 2 && args[0] == "--" {
		return args[1], nil // a report that starts with a dash
	}
	if len(args) != 1 {
		return "", errors.New("hand-back takes one argument")
	}
	a := args[0]
	switch {
	case a == "-":
		// One byte past the limit is enough for the limit to refuse it.
		b, err := io.ReadAll(io.LimitReader(stdin, handBackReadMax))
		return string(b), err
	case strings.HasPrefix(a, "@"):
		b, err := os.ReadFile(a[1:])
		return string(b), err
	case strings.HasPrefix(a, "-"):
		return "", fmt.Errorf("unknown flag %q", a)
	}
	return a, nil
}

// runHandBack records the caller's final report as its session's hand-back
// and prints where it went.
func runHandBack(args []string, stdin io.Reader) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(handBackUsage)
		return 0
	}
	body, err := handBackBody(args, stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: %v\n%s\n", err, handBackUsage)
		return 2
	}
	env, loc, err := caller()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := env.RecordHandBack(ctx, loc, body)
	if err != nil {
		return fail(err)
	}
	out, _ := json.Marshal(map[string]any{"by": h.By, "at": h.At, "bytes": len(h.Body), "view": "hand_back"})
	fmt.Println(string(out))
	return 0
}
