// Package trust derives, for a linked git worktree, the folder trust the user
// already gave its main checkout in a harness's own trust store. It derives
// and never creates: a repository the user never trusted keeps its dialog.
//
// Only Antigravity needs it. Claude Code 2.1.319 and Codex 0.167 resolve a
// linked worktree to its main checkout themselves (Codex in
// codex-rs/git-utils/src/trust.rs); Antigravity keeps one exact directory per
// entry of trustedWorkspaces in ~/.gemini/antigravity-cli/settings.json (a
// trusted parent does not cover its children) and asks again in every
// worktree. ~/.gemini/trustedFolders.json is Gemini CLI's, not Antigravity's.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Result says what Derive found for one harness.
type Result struct {
	Harness string `json:"harness"`
	Store   string `json:"store"`
	// Main is the main checkout the worktree belongs to; "" when dir is not
	// a linked worktree.
	Main string `json:"main,omitempty"`
	// Settings is the value for Antigravity's `--settings` ("for this
	// session only, never saved"): the store's trustedWorkspaces plus dir,
	// since the flag replaces the whole list. "" when there is nothing to
	// derive.
	Settings string `json:"settings,omitempty"`
	Note     string `json:"note"`
}

// Derive extends, for one session, the trust home's Antigravity store gives
// dir's main checkout to dir, when dir is a linked worktree of a trusted
// repository. It only reads: the store is never written, so it cannot race
// the Antigravity processes that rewrite it.
func Derive(home, dir string) (Result, error) {
	r := Result{Harness: "agy", Store: filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return r, err
	}
	main, ok, err := MainCheckout(dir)
	if err != nil {
		return r, err
	}
	if !ok {
		r.Note = "not a linked worktree; the dialog stays the user's"
		return r, nil
	}
	r.Main = main
	raw, err := os.ReadFile(r.Store)
	if errors.Is(err, os.ErrNotExist) {
		r.Note = "no trust store; the user has trusted nothing in Antigravity"
		return r, nil
	}
	if err != nil {
		return r, err
	}
	var settings struct {
		Trusted []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return r, fmt.Errorf("%s: %w", r.Store, err)
	}
	if key, ok := lookup(settings.Trusted, dir); ok {
		r.Note = fmt.Sprintf("already trusted as %s", key)
		return r, nil
	}
	if _, ok := lookup(settings.Trusted, main); !ok {
		r.Note = fmt.Sprintf("main checkout %s is not trusted in Antigravity; the dialog stays the user's", main)
		return r, nil
	}
	out, err := json.Marshal(map[string][]string{"trustedWorkspaces": append(append([]string{}, settings.Trusted...), dir)})
	if err != nil {
		return r, err
	}
	r.Settings = string(out)
	r.Note = fmt.Sprintf("trust of %s extended to its linked worktree %s for this session", main, dir)
	return r, nil
}

// agySubcommands take no session flags.
var agySubcommands = map[string]bool{"acp": true, "agent": true, "agents": true, "changelog": true, "help": true, "install": true,
	"mcp": true, "mic-serve": true, "models": true, "plugin": true, "plugins": true, "remote-control": true, "update": true}

// Args returns argv with `--settings <settings>` after the agy binary when
// argv starts an Antigravity session that names no settings of its own; any
// other command is returned unchanged.
func Args(argv []string, settings string) []string {
	if settings == "" || len(argv) == 0 || filepath.Base(argv[0]) != "agy" {
		return argv
	}
	if len(argv) > 1 && agySubcommands[argv[1]] {
		return argv
	}
	for _, a := range argv[1:] {
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			return argv
		}
	}
	return append([]string{argv[0], "--settings", settings}, argv[1:]...)
}

// lookup finds the entry that names path's directory. Antigravity stores
// each directory as the user opened it, so entries compare by file identity:
// on a case-insensitive volume two spellings are one directory, on a
// case-sensitive one they are two, and an entry that no longer exists names
// nothing.
func lookup(trusted []string, path string) (string, bool) {
	want, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	for _, t := range trusted {
		if info, err := os.Stat(t); err == nil && os.SameFile(info, want) {
			return t, true
		}
	}
	return "", false
}

// MainCheckout resolves a linked worktree to its main checkout by reading
// git's own files, without running git: dir/.git is a file naming a gitdir
// under <common>/worktrees/<name>, whose commondir names <common>, and the
// main checkout is <common>'s parent. ok is false for a main checkout, a
// plain directory, or anything that does not fit that shape.
func MainCheckout(dir string) (main string, ok bool, err error) {
	pointer := filepath.Join(dir, ".git")
	ownInfo, err := os.Lstat(pointer)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !ownInfo.Mode().IsRegular() {
		return "", false, nil
	}
	stat, ok := ownInfo.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return "", false, nil
	}
	raw, err := os.ReadFile(pointer)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && strings.Contains(err.Error(), "is a directory") {
			return "", false, nil
		}
		return "", false, err
	}
	target, found := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !found {
		return "", false, nil
	}
	gitdir := strings.TrimSpace(target)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	gitdir, err = filepath.EvalSymlinks(gitdir)
	if err != nil {
		return "", false, nil
	}
	if filepath.Base(filepath.Dir(gitdir)) != "worktrees" {
		return "", false, nil
	}
	common, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return "", false, nil
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitdir, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil || commonDir != filepath.Dir(filepath.Dir(gitdir)) || filepath.Base(commonDir) != ".git" {
		return "", false, nil
	}
	if info, err := os.Stat(filepath.Join(commonDir, "HEAD")); err != nil || info.IsDir() {
		return "", false, nil
	}
	// Both the registered directory and pointer must match: a pointer alias
	// must never extend trust to a different directory.
	back, err := os.ReadFile(filepath.Join(gitdir, "gitdir"))
	if err != nil {
		return "", false, nil
	}
	registered := strings.TrimSpace(string(back))
	if !filepath.IsAbs(registered) {
		registered = filepath.Join(gitdir, registered)
	}
	registeredDir, err := os.Stat(filepath.Dir(registered))
	if err != nil {
		return "", false, nil
	}
	candidateDir, err := os.Stat(dir)
	if err != nil || !os.SameFile(registeredDir, candidateDir) {
		return "", false, nil
	}
	regInfo, err := os.Lstat(registered)
	if err != nil {
		return "", false, nil
	}
	if !regInfo.Mode().IsRegular() || !os.SameFile(regInfo, ownInfo) {
		return "", false, nil
	}
	return filepath.Dir(commonDir), true, nil
}
