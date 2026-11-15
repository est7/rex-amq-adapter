package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Codex asks the user to review every hook it has not approved before a
// session starts ("Hooks need review"), and runs none of them until they
// answer in /hooks. It records each approval in config.toml as
// [hooks.state."<hooks.json>:<event>:<group>:<hook>"] trusted_hash, the hash
// of the hook's normalized declaration, so a hook rewritten since (an
// adapter upgrade) asks again. These functions read that state; nothing here
// writes it, since approving a hook is the user's call.
//
// The hash is ported from Codex (codex-rs/hooks/src/engine/discovery.rs
// hook_hash, codex-rs/config/src/fingerprint.rs version_for_toml):
// sha256 over the sorted, compact JSON of
// {event_name, matcher, hooks: [normalized handler]}.

// CodexHookReview is one hook in Codex's hooks.json that would stop a new
// Codex session on its review screen.
type CodexHookReview struct {
	Key    string `json:"key"`    // the hooks.state key Codex approves it under
	Status string `json:"status"` // HookUntrusted or HookModified
}

// Why a hook needs review, as Codex's /hooks names it.
const (
	HookUntrusted = "untrusted" // never approved
	HookModified  = "modified"  // changed since it was approved
)

// CodexHooksKey is the path Codex keys the hooks of hooksFile by: as given
// for the default ~/.codex, with every symlink resolved when CODEX_HOME
// named the directory (Codex canonicalizes an override, not the default).
func CodexHooksKey(hooksFile string, fromEnv bool) string {
	if !fromEnv {
		return filepath.Clean(hooksFile)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(hooksFile))
	if err != nil {
		return filepath.Clean(hooksFile)
	}
	return filepath.Join(dir, filepath.Base(hooksFile))
}

// CodexHooksNeedingReview lists the hooks in hooksFile that Codex would ask
// about, keyed under key (CodexHooksKey), against the approvals in config.
// A hooks file that is missing, empty, or that Codex could not parse holds
// no hook Codex loads, so nothing needs review; a config that cannot be read
// is an error, since Codex itself would not start.
func CodexHooksNeedingReview(hooksFile, key, config string) ([]CodexHookReview, error) {
	b, err := os.ReadFile(hooksFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Hooks map[string][]codexMatcherGroup `json:"hooks"`
	}
	if json.Unmarshal(b, &file) != nil {
		return nil, nil
	}
	c, err := readCodexConfig(config)
	if err != nil {
		return nil, err
	}
	approved := codexHookStates(c.doc)
	var out []CodexHookReview
	for _, name := range slices.Sorted(maps.Keys(file.Hooks)) {
		event, ok := codexEvents[name]
		if !ok {
			continue
		}
		for g, group := range file.Hooks[name] {
			matcher := group.Matcher
			if !event.matches {
				matcher = nil
			}
			for h, handler := range group.Hooks {
				id, ok := handler.identity(event, matcher)
				if !ok {
					continue
				}
				k := fmt.Sprintf("%s:%s:%d:%d", key, event.label, g, h)
				trusted, found := approved[k]
				switch {
				case !found:
					out = append(out, CodexHookReview{Key: k, Status: HookUntrusted})
				case trusted != id:
					out = append(out, CodexHookReview{Key: k, Status: HookModified})
				}
			}
		}
	}
	return out, nil
}

// codexHookStates maps each hooks.state key to its trusted_hash.
func codexHookStates(doc map[string]any) map[string]string {
	hooks, _ := doc["hooks"].(map[string]any)
	states, _ := hooks["state"].(map[string]any)
	out := map[string]string{}
	for k, v := range states {
		st, _ := v.(map[string]any)
		if h, ok := st["trusted_hash"].(string); ok {
			out[k] = h
		}
	}
	return out
}

type codexEvent struct {
	label   string // hook_event_key_label
	matches bool   // matcher_pattern_for_event keeps the group's matcher
	// sessionEnd: a timeout defaults to 1 s and is clamped to 1..3 s.
	sessionEnd bool
	// context: additionalContextLimit counts for this event.
	context bool
}

// codexEvents are the hooks.json event names Codex loads (HookEventsToml).
var codexEvents = map[string]codexEvent{
	"PreToolUse":        {label: "pre_tool_use", matches: true, context: true},
	"PermissionRequest": {label: "permission_request", matches: true},
	"PostToolUse":       {label: "post_tool_use", matches: true, context: true},
	"PreCompact":        {label: "pre_compact", matches: true},
	"PostCompact":       {label: "post_compact", matches: true},
	"PreModelRequest":   {label: "pre_model_request"},
	"PostModelRequest":  {label: "post_model_request"},
	"SessionStart":      {label: "session_start", matches: true, context: true},
	"SessionEnd":        {label: "session_end", matches: true, sessionEnd: true},
	"UserPromptSubmit":  {label: "user_prompt_submit", context: true},
	"SubagentStart":     {label: "subagent_start", matches: true, context: true},
	"SubagentStop":      {label: "subagent_stop", matches: true},
	"Stop":              {label: "stop"},
	"Interrupt":         {label: "interrupt", sessionEnd: true},
}

type codexMatcherGroup struct {
	Matcher *string        `json:"matcher"`
	Hooks   []codexHandler `json:"hooks"`
}

type codexHandler struct {
	Type                   string  `json:"type"`
	Command                string  `json:"command"`
	Timeout                *uint64 `json:"timeout"`
	Async                  bool    `json:"async"`
	StatusMessage          *string `json:"statusMessage"`
	AdditionalContextLimit *uint64 `json:"additionalContextLimit"`
}

// codexDefaultContextLimit is DEFAULT_HOOK_OUTPUT_TOKEN_LIMIT: a limit equal
// to it is dropped from the hashed declaration.
const codexDefaultContextLimit = 2500

// identity is the trusted_hash Codex computes for h, and false for a handler
// Codex skips at load (and so never asks about: an empty command, a prompt
// or agent hook) or one whose declaration this port does not model.
func (h codexHandler) identity(e codexEvent, matcher *string) (string, bool) {
	if h.Type != "command" || strings.TrimSpace(h.Command) == "" {
		return "", false
	}
	var timeout uint64
	switch {
	case e.sessionEnd:
		timeout = 1
		if h.Timeout != nil {
			timeout = min(max(*h.Timeout, 1), 3)
		}
	case h.Timeout != nil:
		timeout = max(*h.Timeout, 1)
	default:
		timeout = 600
	}
	handler := map[string]any{"type": "command", "command": h.Command, "timeout": timeout, "async": h.Async}
	if h.StatusMessage != nil {
		handler["statusMessage"] = *h.StatusMessage
	}
	if e.context && h.AdditionalContextLimit != nil && *h.AdditionalContextLimit != codexDefaultContextLimit {
		handler["additionalContextLimit"] = *h.AdditionalContextLimit
	}
	id := map[string]any{"event_name": e.label, "hooks": []any{handler}}
	if matcher != nil {
		id["matcher"] = *matcher
	}
	// encoding/json sorts map keys, as serde_json's sort_all_objects does;
	// unlike serde_json it escapes <, > and & unless told not to.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(id); err != nil {
		return "", false
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:]), true
}
