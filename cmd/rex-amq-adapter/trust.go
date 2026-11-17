package main

import (
	"os"
	"slices"
	"time"

	"github.com/est7/rex-amq-adapter/internal/cli"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

// trustGate is the user's own trust stores and the adapter's book.
func trustGate(c *cli.Ctx) (trust.Gate, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return trust.Gate{}, err
	}
	paths, err := store.DefaultPaths()
	if err != nil {
		return trust.Gate{}, err
	}
	return trust.Gate{Stores: trust.StoresFor(home, c.Getenv), State: paths.Home}, nil
}

// trustCheck is each harness's folder-trust verdict for DIR, read only,
// and the Antigravity --settings value derived for it.
func trustCheck(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	dir := in.Arg("dir")
	g, err := trustGate(c)
	if err != nil {
		return cli.Result{}, err
	}
	var verdicts []trust.Verdict
	for _, k := range []core.Kind{core.Claude, core.Codex, core.Agy, core.Pi} {
		v, err := g.Check(k, dir)
		if err != nil {
			return cli.Result{}, err
		}
		verdicts = append(verdicts, v)
	}
	derived, err := trust.Derive(g.Stores.Home, dir)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Result{Data: map[string]any{"dir": dir, "verdicts": verdicts, "settings": derived.Settings, "note": derived.Note}}, nil
}

// trustGrant is the user's "yes, I trust this folder" for every harness:
// one result per directory; a directory that fails is reported and the
// rest go on (meta.complete=false), and when none was granted the first
// failure is the command's. --dry-run resolves each project and writes
// nothing.
func trustGrant(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	g, err := trustGate(c)
	if err != nil {
		return cli.Result{}, err
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	items := []map[string]any{}
	res := cli.Result{}
	var first error
	failed := func(d string, err error) {
		items = append(items, map[string]any{"dir": d, "status": "failed", "error": err.Error()})
		res.Incomplete = true
		if first == nil {
			first = err
		}
	}
	for _, d := range in.Rest("dir") {
		if in.DryRun() {
			p, err := trust.Resolve(d)
			if err != nil {
				failed(d, err)
				continue
			}
			items = append(items, map[string]any{"dir": p.Dir, "root": p.Root, "status": "would_grant", "git": p.Git()})
			continue
		}
		r, err := g.Grant(ctx, d, "explicit")
		if err != nil {
			failed(d, err)
			continue
		}
		items = append(items, map[string]any{"dir": r.Dir, "root": r.Root, "status": "granted", "source": r.Source, "claude": r.Claude, "codex": r.Codex})
	}
	res.Data = map[string]any{"items": items, "total": len(items), "truncated": false, "dry_run": in.DryRun()}
	if first != nil && !slices.ContainsFunc(items, func(it map[string]any) bool { return it["status"] != "failed" }) {
		return res, first
	}
	return res, nil
}

func trustRootList(c *cli.Ctx, in cli.Input) (cli.Result, error) {
	g, err := trustGate(c)
	if err != nil {
		return cli.Result{}, err
	}
	roots, err := g.Roots()
	if err != nil {
		return cli.Result{}, err
	}
	return rootsResult(roots, false), nil
}

func trustRootAdd(c *cli.Ctx, in cli.Input) (cli.Result, error) { return updateRoots(c, in, true) }

func trustRootRemove(c *cli.Ctx, in cli.Input) (cli.Result, error) { return updateRoots(c, in, false) }

// updateRoots adds or removes declared roots and returns the roots
// afterwards; --dry-run returns what they would be.
func updateRoots(c *cli.Ctx, in cli.Input, add bool) (cli.Result, error) {
	g, err := trustGate(c)
	if err != nil {
		return cli.Result{}, err
	}
	dirs := in.Rest("dir")
	if in.DryRun() {
		preview := g.PreviewRemoveRoots
		if add {
			preview = g.PreviewAddRoots
		}
		roots, err := preview(dirs...)
		if err != nil {
			return cli.Result{}, err
		}
		return rootsResult(roots, true), nil
	}
	ctx, cancel := within(c, 30*time.Second)
	defer cancel()
	var roots []trust.Root
	if add {
		roots, err = g.AddRoots(ctx, dirs...)
	} else {
		roots, err = g.RemoveRoots(ctx, dirs...)
	}
	if err != nil {
		return cli.Result{}, err
	}
	return rootsResult(roots, false), nil
}

func rootsResult(roots []trust.Root, dryRun bool) cli.Result {
	if roots == nil {
		roots = []trust.Root{}
	}
	d := map[string]any{"items": roots, "total": len(roots), "truncated": false}
	if dryRun {
		d["dry_run"] = true
	}
	return cli.Result{Data: d}
}
