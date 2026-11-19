package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/guide"
)

// Every topic and the index fit what one `skill show` may print (§5.1: a
// topic at most 8 KiB, the index at most 1.5 KiB, in JSON and in --text).
func TestSkillTopicSizes(t *testing.T) {
	isolate(t)
	want := []string{"identity", "task-label", "mail", "peers", "crews", "review", "epics", "summon", "rex", "rules"}
	if got := guide.Names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("topics %v, want %v", got, want)
	}
	for _, tp := range guide.Topics() {
		if tp.Size > guide.MaxTopic {
			t.Errorf("topic %s is %d bytes, more than %d", tp.Name, tp.Size, guide.MaxTopic)
		}
		r := invoke(t, "", "skill", "show", tp.Name, "--text")
		if r.code != 0 || r.stdout != tp.Markdown+"\n" && r.stdout != tp.Markdown || len(r.stdout) > guide.MaxTopic {
			t.Errorf("skill show %s --text: %d, %d bytes", tp.Name, r.code, len(r.stdout))
		}
	}
	idx := guide.Index("/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter")
	if len(idx) > guide.MaxIndex {
		t.Errorf("index is %d bytes, more than %d:\n%s", len(idx), guide.MaxIndex, idx)
	}
	for _, args := range [][]string{{"skill", "list"}, {"skill", "show"}} {
		r := invoke(t, "", append(args, "--json")...)
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &env) != nil || len(env.Data) > guide.MaxIndex {
			t.Errorf("%v --json: exit %d, data %d bytes, more than %d", args, r.code, len(env.Data), guide.MaxIndex)
		}
		if r := invoke(t, "", append(args, "--text")...); r.code != 0 || len(r.stdout) > guide.MaxIndex || !strings.Contains(r.stdout, "skill show <topic> --text") {
			t.Errorf("%v --text: exit %d, %d bytes:\n%s", args, r.code, len(r.stdout), r.stdout)
		}
	}
	// A topic this binary does not have, the old whole-guide one included.
	for _, stale := range []string{"guide", "nope"} {
		_, env := machine(t, "skill", "show", stale)
		if env.Error == nil || env.Error.Code != "not_found" || env.Error.Details["kind"] != "topic" || env.Error.NextAction == nil || *env.Error.NextAction != "rex-amq-adapter skill list" {
			t.Errorf("skill show %s: %+v", stale, env.Error)
		}
		if topics, _ := env.Error.Details["topics"].([]any); len(topics) != len(want) {
			t.Errorf("skill show %s lists topics %v", stale, env.Error.Details["topics"])
		}
	}
}

// Every command an agent runs is taught by at least one topic (§3.8), so a
// command added to the registry without a word in the guide fails here.
func TestSkillTopicsTeachEveryAgentCommand(t *testing.T) {
	var all strings.Builder
	for _, tp := range guide.Topics() {
		all.WriteString(tp.Markdown)
	}
	text := all.String()
	for _, c := range registry().Commands {
		if c.Audience != cli.AudienceAgent {
			continue
		}
		if !strings.Contains(text, "rex-amq-adapter "+c.Name()) {
			t.Errorf("no skill topic teaches `rex-amq-adapter %s`", c.Name())
		}
	}
	// No topic teaches a v1 command form, which v:2 refuses.
	v1 := regexp.MustCompile(`rex-amq-adapter (who|env|title|join|crews|summon|await|retire|collect|send|reply|read|drain|thread|trace|dlq|list|link|unlink|view|hand-back|notify-personal|events|panel|setup|status|remove)\b`)
	if m := v1.FindAllString(text, -1); len(m) > 0 {
		t.Errorf("topics teach v1 forms: %v", m)
	}
	if strings.Contains(text, "sol-"+"loop") {
		t.Error("a topic still names the single-seat tier by its old name")
	}
}

// The topic files are the guide's one source: none is left out of the
// embed, and none is a stray copy of another.
func TestSkillTopicFilesAreEmbedded(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "guide", "topics", "*.md"))
	if err != nil || len(files) != len(guide.Topics()) {
		t.Fatalf("%d topic files, %d embedded (%v)", len(files), len(guide.Topics()), err)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "internal", "guide", "skill.md")); err == nil {
		t.Error("internal/guide/skill.md, the old single-file guide, is still in the tree")
	}
}
