package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/fault"
)

// crewDir is the directory whose crews apply: --cwd, else the working one.
func crewDir(in cli.Input) (string, error) {
	if d := in.Str("cwd"); d != "" {
		return d, nil
	}
	return os.Getwd()
}

// crewItem is one crew as `crew list` returns it, with the first sentence
// of its conventions as its purpose.
type crewItem struct {
	Name        string   `json:"name"`
	Lead        string   `json:"lead"`
	Members     []string `json:"members"`
	Roles       []string `json:"roles"`
	Layer       string   `json:"layer"`
	Conventions bool     `json:"conventions"`
	Purpose     string   `json:"purpose"`
}

// roleItem is a role a session of no crew can take.
type roleItem struct {
	Name    string `json:"name"`
	Layer   string `json:"layer"`
	Purpose string `json:"purpose"`
}

// crewPage is the crews, paged, and the roles of a session of no crew.
type crewPage struct {
	cli.List[crewItem]
	Roles []roleItem `json:"roles"`
}

func crewList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	dir, err := crewDir(in)
	if err != nil {
		return cli.Result{}, err
	}
	lib := crew.Load(dir)
	entries, err := lib.List()
	if err != nil {
		return cli.Result{}, err
	}
	crews, roles := []crewItem{}, []roleItem{}
	for _, e := range entries {
		if e.Crew == "" {
			for _, r := range e.Roles {
				p, _, err := lib.Role("", r)
				if err != nil {
					return cli.Result{}, err
				}
				roles = append(roles, roleItem{Name: r, Layer: e.Layer, Purpose: crew.Purpose(p.Text)})
			}
			continue
		}
		it := crewItem{Name: e.Crew, Lead: e.Lead, Members: nonNil(e.Members), Roles: nonNil(e.Roles), Layer: e.Layer, Conventions: e.Conventions}
		if e.Conventions {
			conv, _, err := lib.Conventions(e.Crew)
			if err != nil {
				return cli.Result{}, err
			}
			it.Purpose = crew.Purpose(conv.Text)
		}
		crews = append(crews, it)
	}
	page, err := cli.Page(crews, in, "crew list|"+dir)
	if err != nil {
		return cli.Result{}, err
	}
	data := crewPage{List: page, Roles: roles}
	return cli.Result{Data: data, Human: data.text}, nil
}

func (p crewPage) text(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CREW\tCONVENTIONS\tLEAD\tMEMBERS\tFROM")
	for _, c := range p.Items {
		conv := "-"
		if c.Conventions {
			conv = "yes"
		}
		members := strings.Join(c.Members, ", ")
		if members == "" {
			members = strings.Join(c.Roles, ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, conv, orDash(c.Lead), orDash(members), c.Layer)
	}
	for _, r := range p.Roles {
		fmt.Fprintf(tw, "(any session)\t-\t-\t%s\t%s\n", r.Name, r.Layer)
	}
	return tw.Flush()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// crewShow is, changing nothing, the briefing an agent taking a role in a
// crew where it works would get: the crew's conventions, the role's
// prompt, and the harnesses, models, and efforts it starts on. Without
// --role it is the crew's lead; without a crew, the role in a session of
// no crew.
func crewShow(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	name, role := in.Arg("crew"), in.Str("role")
	if name == "" && role == "" {
		return cli.Result{}, cli.Usage(c.Cmd, "name a CREW, a --role, or both")
	}
	dir, err := crewDir(in)
	if err != nil {
		return cli.Result{}, err
	}
	lib := crew.Load(dir)
	var md strings.Builder
	data := map[string]any{"crew": name, "role": role}
	if name != "" {
		cr, ok, err := lib.Crew(name)
		if err != nil {
			return cli.Result{}, err
		}
		if !ok {
			return cli.Result{}, fault.New(fault.NotFound, "no crew %s in %s", name, dir).With("kind", "crew").WithNext("rex-amq-adapter crew list")
		}
		if role == "" {
			if role = cr.Lead; role == "" {
				return cli.Result{}, cli.Usage(c.Cmd, "crew %s names no lead: pass --role", name)
			}
		}
		data["role"], data["lead"], data["members"] = role, cr.Lead, nonNil(cr.Members)
		fmt.Fprintf(&md, "crew %s (%s): lead %s; members %s\n", name, from(cr.Conventions), orDash(cr.Lead), orDash(strings.Join(cr.Members, ", ")))
		fmt.Fprintf(&md, "\n## Team conventions of crew %s\n\n%s\n", name, cr.Conventions.Text)
	}
	p, ok, err := lib.Role(name, role)
	if err != nil {
		return cli.Result{}, err
	}
	switch {
	case !ok && name == "":
		return cli.Result{}, fault.New(fault.NotFound, "no prompt for role %s", role).With("kind", "role").WithNext("rex-amq-adapter crew list")
	case !ok:
		fmt.Fprintf(&md, "\n## Role %s\n\nno prompt: an agent in this role works from the conventions and its brief\n", role)
	default:
		fmt.Fprintf(&md, "\n## Role %s (%s)\n\n%s\n", role, from(p), p.Text)
	}
	route, err := lib.Routing(role)
	if err != nil {
		return cli.Result{}, err
	}
	source := route.Layer
	if route.Path != "" {
		source += " " + route.Path
	}
	fmt.Fprintf(&md, "\n## Routing of role %s ([role.%q], %s)\n\n", role, route.Role, source)
	var routing []map[string]string
	for _, k := range route.Harnesses {
		l := route.Launch(k)
		fmt.Fprintf(&md, "- %s: model %s, effort %s\n", k, orDefault(l.Model), orDefault(l.Effort))
		routing = append(routing, map[string]string{"harness": string(k), "model": l.Model, "effort": l.Effort})
	}
	data["routing"], data["markdown"] = routing, md.String()
	return cli.Result{Data: data, Markdown: md.String()}, nil
}

func orDefault(s string) string {
	if s == "" {
		return "(the CLI's default)"
	}
	return s
}

func from(p crew.Prompt) string {
	if p.Path == "" {
		return "built-in"
	}
	return p.Path
}
