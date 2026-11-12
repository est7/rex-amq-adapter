// Package crew finds the prompts that tell an agent how its team works: a
// crew's team conventions, shared by every agent in a Rex tab named after
// the crew, and a role prompt for the role its pane label names.
//
// A crew is a directory: team-conventions.md, plus <role>.md for roles that
// work differently in this crew. The conventions may open with a header that
// declares the crew's shape, which is not part of the prompt:
//
//	---
//	lead: implementer
//	members: implementer, reviewer
//	--- roles/<role>.md holds the role prompts used
//
// in any tab. Directories are searched in order, the first file found wins
// whole: the project's .agents/crews (from the agent's directory upwards),
// the user's ~/.config/rex-amq-adapter/crews, then the prompts built in.
package crew

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed builtin
var builtin embed.FS

// Conventions is the file holding a crew's shared conventions.
const Conventions = "team-conventions.md"

// notRoles are files in a crew directory that are not role prompts. A crew's
// goal.md describes a mission (Runner's format); the orchestrator role's prompt is
// roles/goal.md.
var notRoles = map[string]bool{Conventions: true, "goal.md": true, "readme.md": true}

// Prompt is a prompt file's text and where it came from.
type Prompt struct {
	Text string
	// Path is the file read; empty for a built-in prompt.
	Path string
}

// Layer is one directory of crews.
type Layer struct {
	Name string // "project", "user", or "built-in"
	FS   fs.FS
	Dir  string // the directory FS reads; empty when built in
}

// Library is the layers searched, highest precedence first.
type Library []Layer

// Load builds the library for an agent working in dir. A layer whose
// directory does not exist is left out.
func Load(dir string) Library {
	var lib Library
	if p := projectDir(dir); p != "" {
		lib = append(lib, Layer{Name: "project", FS: os.DirFS(p), Dir: p})
	}
	if u := userDir(); u != "" && isDir(u) {
		lib = append(lib, Layer{Name: "user", FS: os.DirFS(u), Dir: u})
	}
	sub, _ := fs.Sub(builtin, "builtin") // a constant, existing directory
	return append(lib, Layer{Name: "built-in", FS: sub})
}

// Crew is a crew's declared shape and its conventions.
type Crew struct {
	Name string
	// Lead is the role that leads it, Members the roles it is made of (a role
	// may repeat); empty when its conventions declare none.
	Lead        string
	Members     []string
	Conventions Prompt
}

// Crew returns crew name: its header and its conventions without it.
func (l Library) Crew(name string) (Crew, bool, error) {
	if name == "" {
		return Crew{}, false, nil
	}
	p, ok, err := l.find(path.Join(name, Conventions))
	if !ok || err != nil {
		return Crew{}, ok, err
	}
	c := Crew{Name: name}
	if c.Lead, c.Members, p.Text, err = parseHeader(p.Text); err != nil {
		return Crew{}, false, fmt.Errorf("%s: %w", path.Join(name, Conventions), err)
	}
	c.Conventions = p
	return c, true, nil
}

// Conventions returns the team conventions of crew, without their header.
func (l Library) Conventions(crew string) (Prompt, bool, error) {
	c, ok, err := l.Crew(crew)
	return c.Conventions, ok, err
}

// parseHeader splits a conventions file into its header's lead and members
// and the text after it. A file without a header declares nothing.
func parseHeader(text string) (lead string, members []string, body string, err error) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", nil, text, nil
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return "", nil, "", errors.New("the header is not closed with ---")
	}
	for _, line := range strings.Split(head, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return "", nil, "", fmt.Errorf("header line %q is not key: value", line)
		}
		switch strings.TrimSpace(key) {
		case "lead":
			lead = Name(value)
		case "members":
			for _, m := range strings.Split(value, ",") {
				if m := Name(m); m != "" {
					members = append(members, m)
				}
			}
		default:
			return "", nil, "", fmt.Errorf("unknown header key %q (want lead or members)", strings.TrimSpace(key))
		}
	}
	return lead, members, strings.TrimSpace(body), nil
}

// Role returns the prompt for role in crew: the crew's own, else the shared
// one. crew may be empty (a tab that names no crew).
func (l Library) Role(crew, role string) (Prompt, bool, error) {
	if role == "" {
		return Prompt{}, false, nil
	}
	if crew != "" && !notRoles[role+".md"] {
		if p, ok, err := l.find(path.Join(crew, role+".md")); ok || err != nil {
			return p, ok, err
		}
	}
	return l.find(path.Join("roles", role+".md"))
}

func (l Library) find(name string) (Prompt, bool, error) {
	for _, layer := range l {
		b, err := fs.ReadFile(layer.FS, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Prompt{}, false, err
		}
		p := Prompt{Text: strings.TrimSpace(string(b))}
		if layer.Dir != "" {
			p.Path = filepath.Join(layer.Dir, filepath.FromSlash(name))
		}
		return p, true, nil
	}
	return Prompt{}, false, nil
}

// Entry is one crew or the shared roles, as listed.
type Entry struct {
	Crew  string // "" for the shared roles
	Roles []string
	// Lead and Members are what the crew's conventions declare.
	Lead    string
	Members []string
	Layer   string
	// Conventions is set when the crew has team conventions.
	Conventions bool
}

// List names every crew and shared role, with the layer each comes from. A
// crew or role shadowed by a higher layer is listed once, from that layer.
func (l Library) List() ([]Entry, error) {
	seen := map[string]bool{}
	var out []Entry
	for _, layer := range l {
		dirs, err := fs.ReadDir(layer.FS, ".")
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			crew := d.Name()
			if crew == "roles" {
				crew = ""
			}
			e := Entry{Crew: crew, Layer: layer.Name}
			files, err := fs.ReadDir(layer.FS, d.Name())
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				name := strings.ToLower(f.Name())
				switch {
				case f.IsDir() || !strings.HasSuffix(name, ".md"):
				case name == Conventions:
					e.Conventions = !seen[crew+"/"+Conventions]
					seen[crew+"/"+Conventions] = true
				case (crew == "" || !notRoles[name]) && !seen[crew+"/"+name]:
					seen[crew+"/"+name] = true
					e.Roles = append(e.Roles, strings.TrimSuffix(name, ".md"))
				}
			}
			if e.Conventions {
				c, _, err := l.Crew(crew)
				if err != nil {
					return nil, err
				}
				e.Lead, e.Members = c.Lead, c.Members
			}
			if len(e.Roles) > 0 || e.Conventions {
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Crew < out[j].Crew })
	return out, nil
}

// Name reads a crew name from a tab label: lowercased, runs of other
// characters folded to "-". Only names of this form are looked up, so a
// label can never reach outside the crews directory.
func Name(label string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(label), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), "-")
}

// TabName reads the crew a crew tab's label names: the part before any
// "|" ("ship | login" names ship), read as Name reads it, so a hyphenated
// crew ("ship-topic") keeps its name.
func TabName(label string) string {
	head, _, _ := strings.Cut(label, "|")
	return Name(head)
}

// projectDir is the nearest .agents/crews at or above dir.
func projectDir(dir string) string {
	if dir == "" {
		return ""
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if c := filepath.Join(d, ".agents", "crews"); isDir(c) {
			return c
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func userDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rex-amq-adapter", "crews")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Purpose is what a crew or role is for, as a listing shows it: the opening
// sentences of its prompt's first paragraph, on one line, until they say more
// than who it is ("You are QA." alone says nothing of the job).
func Purpose(text string) string {
	para, _, _ := strings.Cut(strings.TrimSpace(text), "\n\n")
	para = strings.Join(strings.Fields(para), " ")
	end := 0
	for end < len(para) && end < minPurpose {
		i := strings.Index(para[end:], ". ")
		if i < 0 {
			return para
		}
		end += i + 1
	}
	return para[:end]
}

// minPurpose is how long a purpose runs before it may end at a sentence.
const minPurpose = 40
