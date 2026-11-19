package main

import (
	"strings"

	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/guide"
)

// skillTopic is one topic of the agent guide. The guide is one topic until
// it is split by subject; `skill show` without a topic is the whole guide.
type skillTopic struct {
	Topic   string `json:"topic"`
	Summary string `json:"summary"`
	Bytes   int    `json:"bytes"`
	text    string
}

func skillTopics() []skillTopic {
	return []skillTopic{{Topic: "guide", Summary: "the whole agent guide: identity, mail, summon, Epics, crews, and review", Bytes: len(guide.Skill), text: guide.Skill}}
}

func skillList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	topics := skillTopics()
	return cli.Result{Data: cli.List[skillTopic]{Items: topics, Total: len(topics)}}, nil
}

func skillShow(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	name := in.Arg("topic")
	if name == "" {
		name = "guide"
	}
	var names []string
	for _, t := range skillTopics() {
		if t.Topic == name {
			return cli.Result{Data: map[string]any{"topic": t.Topic, "markdown": t.text}, Markdown: t.text}, nil
		}
		names = append(names, t.Topic)
	}
	return cli.Result{}, fault.New(fault.NotFound, "no skill topic %s; the topics are %s", name, strings.Join(names, ", ")).
		With("kind", "topic").With("topics", names).WithNext("rex-amq-adapter skill list")
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
