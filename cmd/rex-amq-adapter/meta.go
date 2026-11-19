package main

import (
	"strings"

	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/guide"
)

// skillTopic is one topic of the agent guide as `skill list` lists it.
type skillTopic struct {
	Topic   string `json:"topic"`
	Summary string `json:"summary"`
	Bytes   int    `json:"bytes"`
}

// skillList is the guide's index: its topics, and the same index as
// markdown for --text.
func skillList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	var items []skillTopic
	for _, t := range guide.Topics() {
		items = append(items, skillTopic{Topic: t.Name, Summary: t.Summary, Bytes: t.Size})
	}
	return cli.Result{Data: cli.List[skillTopic]{Items: items, Total: len(items)}, Human: line(guide.Index("rex-amq-adapter"))}, nil
}

// skillShow prints one topic, or without a topic the index. A topic the
// binary does not have (a stub installed by an older one names it) is
// not_found with the current topics.
func skillShow(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	name := in.Arg("topic")
	if name == "" {
		md := guide.Index("rex-amq-adapter")
		return cli.Result{Data: map[string]any{"topic": "", "topics": guide.Names(), "markdown": md}, Markdown: md}, nil
	}
	t, ok := guide.Find(name)
	if !ok {
		names := guide.Names()
		return cli.Result{}, fault.New(fault.NotFound, "no skill topic %s; the topics are %s", name, strings.Join(names, ", ")).
			With("kind", "topic").With("topics", names).WithNext("rex-amq-adapter skill list")
	}
	return cli.Result{Data: map[string]any{"topic": t.Name, "markdown": t.Markdown}, Markdown: t.Markdown}, nil
}

// schemaCmd describes every public command, one command, or the error
// codes. It reads nothing but the registry: no home, config, or PATH.
func schemaCmd(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	r := registry()
	words := in.Rest("command")
	switch {
	case len(words) == 0:
		return cli.Result{Data: r.Schema()}, nil
	case len(words) == 1 && words[0] == "errors":
		return cli.Result{Data: map[string]any{"errors": fault.Table()}}, nil
	}
	if cmd := r.Find(words...); cmd != nil {
		return cli.Result{Data: cmd.Info()}, nil
	}
	return cli.Result{}, cli.Usage(c.Cmd, "no command %q: `rex-amq-adapter schema` lists every command", strings.Join(words, " "))
}

func versionCmd(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	v := version()
	return cli.Result{Data: map[string]any{"version": v}, Human: line(v)}, nil
}
