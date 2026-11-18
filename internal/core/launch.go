package core

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Launch is the model and reasoning effort an agent is started on in its
// harness's CLI; an empty field leaves that choice to the CLI. A Launch is
// only built by ParseLaunch, so both values reach a command line checked:
// a model never starts with "-", so it can never be read as a flag.
type Launch struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// Efforts are the reasoning-effort levels a routing table may name, lowest
// first. Each CLI takes a subset (harnessEfforts).
var Efforts = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// harnessEfforts is what each CLI's effort flag accepts, from its --help
// (claude 2.x and agy: low..max; pi --thinking: off..max) and codex-cli
// 0.169.0's ReasoningEffort, which spells off "none".
var harnessEfforts = map[Kind][]string{
	Claude: {"low", "medium", "high", "xhigh", "max"},
	Agy:    {"low", "medium", "high", "xhigh", "max"},
	Codex:  {"off", "minimal", "low", "medium", "high", "xhigh", "max"},
	Pi:     {"off", "minimal", "low", "medium", "high", "xhigh", "max"},
}

var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// ParseLaunch checks model and effort for kind's CLI: a model of letters,
// digits and ._:/- that starts with a letter or digit, and an effort among
// Efforts that the CLI takes.
func ParseLaunch(kind Kind, model, effort string) (Launch, error) {
	if model != "" && !modelRe.MatchString(model) {
		return Launch{}, fmt.Errorf("model %q is not a model name (want %s)", model, modelRe)
	}
	if effort != "" {
		if !slices.Contains(Efforts, effort) {
			return Launch{}, fmt.Errorf("effort %q is not one of %s", effort, strings.Join(Efforts, "|"))
		}
		allowed, ok := harnessEfforts[kind]
		if !ok {
			return Launch{}, fmt.Errorf("unsupported harness %q", kind)
		}
		if !slices.Contains(allowed, effort) {
			return Launch{}, fmt.Errorf("effort %q is not one %s takes (%s)", effort, kind, strings.Join(allowed, "|"))
		}
	}
	return Launch{Model: model, Effort: effort}, nil
}

// Args returns argv, a command of kind's CLI, with l's model and effort
// flags after its program, where each CLI takes them for a start and a
// resume alike: claude and agy --model/--effort, codex -m and
// -c model_reasoning_effort=, pi --model/--thinking.
func (l Launch) Args(kind Kind, argv []string) []string {
	if len(argv) == 0 || l == (Launch{}) {
		return argv
	}
	var flags []string
	switch kind {
	case Claude, Agy:
		flags = pair(flags, "--model", l.Model)
		flags = pair(flags, "--effort", l.Effort)
	case Codex:
		flags = pair(flags, "-m", l.Model)
		effort := l.Effort
		if effort == "off" {
			effort = "none"
		}
		if effort != "" {
			flags = append(flags, "-c", "model_reasoning_effort="+effort)
		}
	case Pi:
		flags = pair(flags, "--model", l.Model)
		flags = pair(flags, "--thinking", l.Effort)
	}
	out := append([]string{argv[0]}, flags...)
	return append(out, argv[1:]...)
}

func pair(flags []string, flag, value string) []string {
	if value == "" {
		return flags
	}
	return append(flags, flag, value)
}

// Model families. A review by the same family misses what that family
// misses, so review roles start on another family than their caller's.
const (
	Anthropic = "anthropic"
	OpenAI    = "openai"
	Google    = "google"
)

// familyPrefixes map a model name's prefix to its family. "fable" is the
// Claude CLI's alias for its latest model.
var familyPrefixes = []struct{ prefix, family string }{
	{"claude", Anthropic}, {"opus", Anthropic}, {"sonnet", Anthropic}, {"haiku", Anthropic}, {"fable", Anthropic},
	{"gpt", OpenAI}, {"o1", OpenAI}, {"o3", OpenAI}, {"o4", OpenAI},
	{"gemini", Google},
}

// Family is the model family an agent of kind runs on model: the family its
// name starts with (after any provider/ prefix, as Pi writes them), else
// the harness's own (Claude Code Anthropic, Codex OpenAI, Antigravity
// Google). "" is unknown (Pi on its user's default), which never counts as
// another family than anyone's.
func Family(kind Kind, model string) string {
	name := strings.ToLower(model)
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, p := range familyPrefixes {
		if strings.HasPrefix(name, p.prefix) {
			return p.family
		}
	}
	switch kind {
	case Claude:
		return Anthropic
	case Codex:
		return OpenAI
	case Agy:
		return Google
	}
	return ""
}

// CrossFamily reports whether role reviews others' work, and so starts by
// default on another model family than the agent that summons it.
func CrossFamily(role string) bool {
	switch role {
	case "reviewer", "editor", "qa":
		return true
	}
	return false
}

// ReadOnly reports whether role only reads the checkout it works in: the
// review roles and the architect, which designs and never edits product
// code. Seats of a read-only role on distinct harnesses may share one
// directory, and only a read-only role sits a council.
func ReadOnly(role string) bool {
	return CrossFamily(role) || role == "architect"
}
