package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// viewArgs is a parsed `view` command: print a session's view (verb ""),
// rebuild the caller's (refresh), or read messages of its session by id
// (read).
type viewArgs struct {
	verb    string
	session string
	summary string
	ids     []string
}

const viewUsage = "usage: rex-amq-adapter view [--session S] | view refresh [--summary TEXT|@FILE] | view read --ids ID[,ID...]"

func parseViewArgs(args []string) (viewArgs, error) {
	var v viewArgs
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		v.verb, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ids string
	switch v.verb {
	case "":
		fs.StringVar(&v.session, "session", "", "the Epic session to read (a personal agent's linked Epic)")
	case "refresh":
		fs.StringVar(&v.summary, "summary", "", "the narrator's reading of the session, or @FILE")
	case "read":
		fs.StringVar(&ids, "ids", "", "comma-separated message ids")
	default:
		return v, fmt.Errorf("unknown view command %q", v.verb)
	}
	if err := fs.Parse(args); err != nil {
		return v, err
	}
	if fs.NArg() > 0 {
		return v, fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	if v.verb == "read" {
		for _, id := range strings.Split(ids, ",") {
			if id = strings.TrimSpace(id); id != "" {
				v.ids = append(v.ids, id)
			}
		}
		if len(v.ids) == 0 {
			return v, errors.New("view read needs --ids")
		}
	}
	if file, ok := strings.CutPrefix(v.summary, "@"); ok {
		b, err := os.ReadFile(file)
		if err != nil {
			return v, err
		}
		v.summary = strings.TrimSpace(string(b))
	}
	return v, nil
}

// runView prints, rebuilds, or reads from a session's view
// (extensions/rex-amq-adapter/sessions/<session>/view.json).
func runView(args []string) int {
	v, err := parseViewArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: %v\n%s\n", err, viewUsage)
		return 2
	}
	env, loc, err := caller()
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var out any
	switch v.verb {
	case "":
		b, err := env.ReadSessionView(ctx, loc, v.session)
		if err != nil {
			return fail(err)
		}
		_, err = os.Stdout.Write(b)
		if err != nil {
			return fail(err)
		}
		return 0
	case "refresh":
		out, err = env.RefreshSessionView(ctx, loc, v.summary)
	case "read":
		out, err = env.ReadSessionMessages(ctx, loc, v.ids)
	}
	if err != nil {
		return fail(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return fail(err)
	}
	return 0
}
