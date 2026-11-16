package core

import (
	"slices"
	"strings"
	"testing"
)

// Each CLI gets its own flags after its program, for a start and a resume.
func TestLaunchArgsPerHarness(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		l    Launch
		argv []string
		want []string
	}{
		{Claude, Launch{"opus", "high"}, []string{"claude", "--", "brief"}, []string{"claude", "--model", "opus", "--effort", "high", "--", "brief"}},
		{Claude, Launch{"haiku", ""}, []string{"claude", "--resume", "s", "p"}, []string{"claude", "--model", "haiku", "--resume", "s", "p"}},
		{Codex, Launch{"gpt-6-astra", "xhigh"}, []string{"codex", "--no-daemon", "resume", "t", "p"}, []string{"codex", "-m", "gpt-6-astra", "-c", "model_reasoning_effort=xhigh", "--no-daemon", "resume", "t", "p"}},
		{Codex, Launch{"", "off"}, []string{"codex", "--no-daemon", "--", "b"}, []string{"codex", "-c", "model_reasoning_effort=none", "--no-daemon", "--", "b"}},
		{Agy, Launch{"gemini-4-carbon-high", "high"}, []string{"agy", "-i", "b"}, []string{"agy", "--model", "gemini-4-carbon-high", "--effort", "high", "-i", "b"}},
		{Pi, Launch{"anthropic/claude-sonnet-4", "minimal"}, []string{"pi", "--", "b"}, []string{"pi", "--model", "anthropic/claude-sonnet-4", "--thinking", "minimal", "--", "b"}},
		{Claude, Launch{}, []string{"claude", "--", "b"}, []string{"claude", "--", "b"}},
		{Claude, Launch{"opus", "high"}, nil, nil},
	} {
		if got := tc.l.Args(tc.kind, tc.argv); !slices.Equal(got, tc.want) {
			t.Errorf("%s %+v: %q, want %q", tc.kind, tc.l, got, tc.want)
		}
	}
}

// A model that starts with "-" or holds anything but name characters is
// refused, as is an effort the CLI does not take.
func TestLaunchParseRejectsFlagPrefixedModels(t *testing.T) {
	for _, m := range []string{"-m", "--model", "-", "--dangerously-skip-permissions", "opus high", "opus;rm", "a\nb", ".hidden", "claude-opus-4-1@20250805", strings.Repeat("a", 129)} {
		if _, err := ParseLaunch(Claude, m, ""); err == nil {
			t.Errorf("model %q accepted", m)
		}
	}
	for _, tc := range []struct {
		kind   Kind
		effort string
	}{{Claude, "off"}, {Claude, "minimal"}, {Agy, "off"}, {Codex, "ultra"}, {Pi, "none"}, {Claude, "HIGH"}, {Kind("gpt"), "high"}} {
		if _, err := ParseLaunch(tc.kind, "", tc.effort); err == nil {
			t.Errorf("%s effort %q accepted", tc.kind, tc.effort)
		}
	}
	for _, tc := range []struct {
		kind          Kind
		model, effort string
	}{{Claude, "opus", "max"}, {Codex, "gpt-6-astra", "minimal"}, {Pi, "openai/gpt-6.1-sol:high", "off"}, {Agy, "gemini-pro-orchestrator", ""}} {
		if l, err := ParseLaunch(tc.kind, tc.model, tc.effort); err != nil || l != (Launch{tc.model, tc.effort}) {
			t.Errorf("%s %s/%s: %+v %v", tc.kind, tc.model, tc.effort, l, err)
		}
	}
}

// The family follows the model's name, then the harness: Antigravity
// hosting a Claude model is Anthropic, and Pi on its own default is unknown.
func TestFamilyFollowsTheModel(t *testing.T) {
	for _, tc := range []struct {
		kind  Kind
		model string
		want  string
	}{
		{Claude, "", Anthropic}, {Codex, "", OpenAI}, {Agy, "", Google}, {Pi, "", ""},
		{Agy, "claude-opus-4", Anthropic}, {Agy, "gemini-4-carbon-high", Google}, {Agy, "gpt-oss-120b", OpenAI},
		{Claude, "Opus", Anthropic}, {Claude, "fable", Anthropic}, {Codex, "o3", OpenAI}, {Codex, "o4-mini", OpenAI},
		{Pi, "openai/gpt-6.1-sol", OpenAI}, {Pi, "anthropic/claude-sonnet-4", Anthropic}, {Pi, "sonnet:high", Anthropic}, {Pi, "llama-4", ""},
		{Codex, "llama-4", OpenAI},
	} {
		if got := Family(tc.kind, tc.model); got != tc.want {
			t.Errorf("Family(%s, %q) = %q, want %q", tc.kind, tc.model, got, tc.want)
		}
	}
}
