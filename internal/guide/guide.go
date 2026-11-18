// Package guide embeds the agent guide as topics, so upgrading the binary
// updates what agents read. Each topic is a markdown file in topics/ whose
// frontmatter names it (topic) and says what it teaches (summary); the index
// that `skill list`, `skill show`, and the installed SKILL.md stub carry is
// generated from that frontmatter, so it cannot drift from the topics.
package guide

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed topics/*.md
var files embed.FS

// Topic is one topic of the guide.
type Topic struct {
	Name    string
	Summary string
	// Markdown is the topic's text after its frontmatter.
	Markdown string
	// Size is the bytes of the whole file, frontmatter included.
	Size int
}

// MaxTopic and MaxIndex bound what one `skill show` prints: a topic, and the
// index of every topic.
const (
	MaxTopic = 8 << 10
	MaxIndex = 1536
)

var topics = mustLoad()

// Topics is every topic, in the order its files are named.
func Topics() []Topic { return append([]Topic(nil), topics...) }

// Find is the topic called name.
func Find(name string) (Topic, bool) {
	for _, t := range topics {
		if t.Name == name {
			return t, true
		}
	}
	return Topic{}, false
}

// Names is every topic's name.
func Names() []string {
	out := make([]string, len(topics))
	for i, t := range topics {
		out[i] = t.Name
	}
	return out
}

// Index is the guide's table of contents as markdown: one line per topic
// and how to read one, with bin the command that prints it.
func Index(bin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run `%s skill show <topic> --text` for one topic:\n\n", bin)
	for _, t := range topics {
		fmt.Fprintf(&b, "- `%s`: %s\n", t.Name, t.Summary)
	}
	return b.String()
}

// mustLoad reads the embedded topics. They are built into the binary, so a
// malformed one is a build defect, caught by this package's tests.
func mustLoad() []Topic {
	paths, err := fs.Glob(files, "topics/*.md")
	if err != nil {
		panic(err)
	}
	sort.Strings(paths)
	var out []Topic
	for _, p := range paths {
		b, err := files.ReadFile(p)
		if err != nil {
			panic(err)
		}
		t, err := parse(string(b))
		if err != nil {
			panic(fmt.Sprintf("guide topic %s: %v", p, err))
		}
		out = append(out, t)
	}
	return out
}

// parse splits a topic file into its frontmatter's topic and summary and
// its markdown.
func parse(text string) (Topic, error) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Topic{}, fmt.Errorf("no frontmatter")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Topic{}, fmt.Errorf("unterminated frontmatter")
	}
	t := Topic{Markdown: strings.TrimLeft(body, "\n"), Size: len(text)}
	for _, line := range strings.Split(head, "\n") {
		key, value, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(key) {
		case "topic":
			t.Name = strings.TrimSpace(value)
		case "summary":
			t.Summary = strings.TrimSpace(value)
		default:
			return Topic{}, fmt.Errorf("unknown frontmatter key %q", key)
		}
	}
	if t.Name == "" || t.Summary == "" {
		return Topic{}, fmt.Errorf("frontmatter needs topic and summary")
	}
	return t, nil
}
