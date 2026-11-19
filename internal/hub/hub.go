// Package hub resolves the user's Hub directories, the places a personal
// agent works from, and keeps the configured list. Resolution takes the
// first source that names any: ME_HUB (as me-cli reads it), the hub_dirs of
// $XDG_CONFIG_HOME/rex-amq-adapter/config.toml (which Add and Remove keep),
// then $HOME/me when that directory exists. Every path is made absolute and
// has its symlinks resolved, so a repository linked under the hub is not in
// it.
package hub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/pelletier/go-toml/v2"

	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// Sources of a Hub directory.
const (
	SourceEnv    = "ME_HUB"
	SourceConfig = "config"
	SourceHome   = "home"
)

// Dir is one resolved Hub directory.
type Dir struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// ConfigPath is the adapter's config file.
func ConfigPath(getenv func(string) string) (string, error) {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		home := getenv("HOME")
		if home == "" {
			return "", errors.New("neither XDG_CONFIG_HOME nor HOME is set")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rex-amq-adapter", "config.toml"), nil
}

// Canonical is dir made absolute with its symlinks resolved.
func Canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Dirs resolves the Hub directories.
func Dirs(getenv func(string) string) ([]Dir, error) {
	if v := getenv("ME_HUB"); v != "" {
		p, err := Canonical(v)
		if err != nil {
			return nil, fmt.Errorf("ME_HUB=%q: %w", v, err)
		}
		return []Dir{{Path: p, Source: SourceEnv}}, nil
	}
	configured, err := Configured(getenv)
	if err != nil {
		return nil, err
	}
	if len(configured) > 0 {
		out := make([]Dir, 0, len(configured))
		for _, c := range configured {
			p, err := Canonical(c)
			if err != nil {
				continue // a configured hub that is gone resolves to nothing
			}
			out = append(out, Dir{Path: p, Source: SourceConfig})
		}
		return out, nil
	}
	if home := getenv("HOME"); home != "" {
		if p, err := Canonical(filepath.Join(home, "me")); err == nil {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				return []Dir{{Path: p, Source: SourceHome}}, nil
			}
		}
	}
	return []Dir{}, nil
}

type config map[string]any

func readConfig(path string) (config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config{}, nil
	}
	if err != nil {
		return nil, err
	}
	c := config{}
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c config) hubDirs() []string {
	raw, _ := c["hub_dirs"].([]any)
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Configured is the hub_dirs the config file lists, as written there.
func Configured(getenv func(string) string) ([]string, error) {
	path, err := ConfigPath(getenv)
	if err != nil {
		return nil, err
	}
	c, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	return c.hubDirs(), nil
}

// Change is what Add or Remove did, or would do under dry run.
type Change struct {
	Path    string   `json:"path"`
	Config  string   `json:"config"`
	Changed bool     `json:"changed"`
	HubDirs []string `json:"hub_dirs"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// Add lists dir (canonical) in hub_dirs; listing it again changes nothing.
func Add(getenv func(string) string, dir string, dryRun bool) (Change, error) {
	return update(getenv, dir, dryRun, func(dirs []string, p string) []string {
		if slices.Contains(dirs, p) {
			return dirs
		}
		return append(dirs, p)
	})
}

// Remove drops dir (canonical) from hub_dirs; dropping one not listed
// changes nothing.
func Remove(getenv func(string) string, dir string, dryRun bool) (Change, error) {
	return update(getenv, dir, dryRun, func(dirs []string, p string) []string {
		return slices.DeleteFunc(slices.Clone(dirs), func(d string) bool { return d == p })
	})
}

func update(getenv func(string) string, dir string, dryRun bool, f func([]string, string) []string) (Change, error) {
	p, err := Canonical(dir)
	if err != nil {
		return Change{}, err
	}
	path, err := ConfigPath(getenv)
	if err != nil {
		return Change{}, err
	}
	c, err := readConfig(path)
	if err != nil {
		return Change{}, err
	}
	before := c.hubDirs()
	after := f(before, p)
	ch := Change{Path: p, Config: path, Changed: !slices.Equal(before, after), HubDirs: after, DryRun: dryRun}
	if ch.HubDirs == nil {
		ch.HubDirs = []string{}
	}
	if !ch.Changed || dryRun {
		return ch, nil
	}
	list := make([]any, len(after))
	for i, d := range after {
		list[i] = d
	}
	c["hub_dirs"] = list
	b, err := toml.Marshal(c)
	if err != nil {
		return ch, err
	}
	return ch, fsutil.WriteAtomic(path, b)
}
