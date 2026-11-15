package crew

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// RoutingFile is the file in a crews directory that routes roles to a
// harness, a model, and an effort (builtin/routing.toml documents it).
const RoutingFile = "routing.toml"

// AnyRole is the routing table for a role with none of its own.
const AnyRole = "*"

// Route is how a role starts: on the first of Harnesses that fits, with
// that harness's Launch.
type Route struct {
	// Role is the table that routes it: the role's own name, or AnyRole.
	Role      string
	Harnesses []core.Kind
	Launches  map[core.Kind]core.Launch
	// Layer and Path are where the table comes from; Path is empty for the
	// built-in file.
	Layer, Path string
}

// Launch is what kind runs the role on; a harness without a table of its
// own gets the CLI's defaults.
func (r Route) Launch(kind core.Kind) core.Launch { return r.Launches[kind] }

// Routing returns role's route: the first layer with a table for role wins
// it whole; without one in any layer, the first layer's [role."*"]. Every
// layer's routing.toml is read and checked, so a broken file refuses every
// launch instead of a lower layer standing in for it unseen.
func (l Library) Routing(role string) (Route, error) {
	var own, fallback *Route
	for _, layer := range l {
		routes, err := readRouting(layer)
		if err != nil {
			return Route{}, err
		}
		if r, ok := routes[role]; ok && own == nil && role != "" {
			own = &r
		}
		if r, ok := routes[AnyRole]; ok && fallback == nil {
			fallback = &r
		}
	}
	switch {
	case own != nil:
		return *own, nil
	case fallback != nil:
		return *fallback, nil
	}
	return Route{}, fmt.Errorf("no routing for role %q and no [role.%q] in any %s", role, AnyRole, RoutingFile)
}

type routingTable struct {
	Role map[string]roleTable `toml:"role"`
}

type roleTable struct {
	Harness []string     `toml:"harness"`
	Claude  *launchTable `toml:"claude"`
	Codex   *launchTable `toml:"codex"`
	Agy     *launchTable `toml:"agy"`
	Pi      *launchTable `toml:"pi"`
}

type launchTable struct {
	Model  string `toml:"model"`
	Effort string `toml:"effort"`
}

// readRouting reads layer's routing.toml, if it has one: strictly, so a
// misspelt key is an error, and every model and effort checked for its CLI.
func readRouting(layer Layer) (map[string]Route, error) {
	b, err := fs.ReadFile(layer.FS, RoutingFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	path := "built-in " + RoutingFile
	if layer.Dir != "" {
		path = filepath.Join(layer.Dir, RoutingFile)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var t routingTable
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]Route{}
	for role, rt := range t.Role {
		r, err := rt.route(role)
		if err != nil {
			return nil, fmt.Errorf("%s: [role.%q]: %w", path, role, err)
		}
		r.Layer = layer.Name
		if layer.Dir != "" {
			r.Path = filepath.Join(layer.Dir, RoutingFile)
		}
		out[role] = r
	}
	return out, nil
}

func (rt roleTable) route(role string) (Route, error) {
	if role != AnyRole && (role == "" || Name(role) != role) {
		return Route{}, fmt.Errorf("a role is lowercase letters, digits and dashes, or %q", AnyRole)
	}
	if len(rt.Harness) == 0 {
		return Route{}, errors.New("harness names no harness")
	}
	r := Route{Role: role, Launches: map[core.Kind]core.Launch{}}
	for _, h := range rt.Harness {
		k, err := core.ParseKind(h)
		if err != nil {
			return Route{}, err
		}
		if slices.Contains(r.Harnesses, k) {
			return Route{}, fmt.Errorf("harness lists %s twice", k)
		}
		r.Harnesses = append(r.Harnesses, k)
	}
	for k, t := range map[core.Kind]*launchTable{core.Claude: rt.Claude, core.Codex: rt.Codex, core.Agy: rt.Agy, core.Pi: rt.Pi} {
		if t == nil {
			continue
		}
		l, err := core.ParseLaunch(k, t.Model, t.Effort)
		if err != nil {
			return Route{}, fmt.Errorf("%s: %w", k, err)
		}
		r.Launches[k] = l
	}
	return r, nil
}
