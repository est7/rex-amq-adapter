package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
)

const notifyUsage = "usage: rex-amq-adapter notify-personal --ref inbox:<id>|timer:<id> --event-id <id> [--hub DIR] [--personal HANDLE] [--summary TEXT]"

// runNotifyPersonal rings the live personal agent for an outside producer's
// event (app.NotifyPersonal) and prints the result as JSON. It runs anywhere,
// a launchd job included: it acts as no agent, so it resolves no caller.
// Exit 0 for notified and for no_live_personal, 2 for a malformed call, 1
// for a refusal or failure.
func runNotifyPersonal(args []string) int {
	fs := flag.NewFlagSet("notify-personal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var r app.NotifyRequest
	fs.StringVar(&r.Ref, "ref", "", "the producer's entry: inbox:<id> or timer:<id>")
	fs.StringVar(&r.EventID, "event-id", "", "the event's id; a repeat is not sent again")
	fs.StringVar(&r.Hub, "hub", "", "ring only a personal agent working in DIR or below it")
	fs.StringVar(&r.Personal, "personal", "", "ring only the personal agent with this handle")
	fs.StringVar(&r.Summary, "summary", "", "a one-line hint, delivered quoted as untrusted data")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(notifyUsage)
			return 0
		}
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: %v\n%s\n", err, notifyUsage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: unexpected %q\n%s\n", fs.Arg(0), notifyUsage)
		return 2
	}
	n, err := app.ParseNotify(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rex-amq-adapter: %v\n%s\n", err, notifyUsage)
		return 2
	}
	env, err := app.NewEnv(os.Getenv("REX_SERVER"))
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := env.NotifyPersonal(ctx, n)
	if res.Status != "" {
		out, merr := json.Marshal(res)
		if merr != nil {
			return fail(merr)
		}
		fmt.Println(string(out))
	}
	if err != nil {
		return fail(err)
	}
	return 0
}
