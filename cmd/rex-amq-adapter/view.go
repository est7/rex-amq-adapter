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

	"github.com/est7/rex-amq-adapter/internal/app"
)

// viewArgs is a parsed `view` command: print a session's view (verb ""),
// rebuild the caller's (refresh), or read messages of its session by id
// (read). cockpit reads a personal agent's linked Epics instead, named by
// personal or hub when the caller is not that agent.
type viewArgs struct {
	verb     string
	session  string
	summary  string
	ids      []string
	cockpit  bool
	personal string
	hub      string
}

const viewUsage = "usage: rex-amq-adapter view [--session S] [--cockpit [--personal H] [--hub DIR]] | view refresh [--summary TEXT|@FILE] | view read --ids ID[,ID...]"

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
		fs.BoolVar(&v.cockpit, "cockpit", false, "read a personal agent's linked Epics, read-only, from its pane or from outside Rex")
		fs.StringVar(&v.personal, "personal", "", "with --cockpit: the personal agent whose Epics to read")
		fs.StringVar(&v.hub, "hub", "", "with --cockpit: the directory the personal agent works in")
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
	if !v.cockpit && (v.personal != "" || v.hub != "") {
		return v, errors.New("--personal and --hub name a cockpit's scope: they need --cockpit")
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
	if v.cockpit {
		return runCockpit(v)
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

// runCockpit prints a personal agent's linked Epics, or one of them
// (app.Env.ReadCockpitView). It resolves its caller itself: it runs from a
// personal agent's pane or from outside Rex, where caller() has no pane.
func runCockpit(v viewArgs) int {
	env, err := app.NewEnv(os.Getenv("REX_SERVER"))
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := env.ReadCockpitView(ctx, os.Getenv, app.CockpitViewRequest{Session: v.session, Personal: v.personal, Hub: v.hub})
	if errors.Is(err, app.ErrUsage) {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: %v\n", err)
		return 2
	}
	if err != nil {
		return fail(err)
	}
	if _, err := os.Stdout.Write(b); err != nil {
		return fail(err)
	}
	return 0
}
