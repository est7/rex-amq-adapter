// Package app runs the adapter's entry points: agent hooks, the injector amq
// wake calls, and the commands agents and users run. Policy lives in core;
// this package observes, asks core, and acts.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Terminal is the part of Rex the adapter drives; rex.Client implements it.
type Terminal interface {
	Whoami(ctx context.Context) (rex.Server, error)
	Foreground(ctx context.Context, block string) (rex.Process, bool, error)
	ProgramStatus(ctx context.Context, block string) (rex.Status, bool, error)
	CaptureVT(ctx context.Context, block string) (string, error)
	Write(ctx context.Context, block, data string) error
	SendKeys(ctx context.Context, block string, keys ...string) error
	InspectBlock(ctx context.Context, block string) (rex.Block, error)
	InspectSession(ctx context.Context, session string) (rex.Session, error)
	ListBlocks(ctx context.Context, session string) ([]rex.Block, error)
	RenameBlock(ctx context.Context, block, label string) error
}

// Env is everything an entry point needs.
type Env struct {
	Paths store.Paths
	Store store.Store
	// Rex talks to the caller's server; RexOn to the server a record names.
	Rex   Terminal
	RexOn func(server string) Terminal
	// Crews finds crew and role prompts for an agent working in a
	// directory; nil finds none.
	Crews   func(dir string) crew.Library
	AmqBin  string
	SelfBin string // SelfPath(): absolute, stable across Homebrew upgrades
	Now     func() time.Time
}

// NewEnv resolves binaries and state paths. server is the Rex address to
// talk to; "" means rex's default (REX_SERVER in a Rex terminal).
func NewEnv(server string) (Env, error) {
	paths, err := store.DefaultPaths()
	if err != nil {
		return Env{}, err
	}
	return NewEnvAt(server, paths)
}

// NewEnvAt is NewEnv with an explicit state directory.
func NewEnvAt(server string, paths store.Paths) (Env, error) {
	rexBin, err := rex.ResolveBin()
	if err != nil {
		return Env{}, err
	}
	amqBin, err := amq.ResolveBin()
	if err != nil {
		return Env{}, err
	}
	self, err := SelfPath()
	if err != nil {
		return Env{}, err
	}
	return Env{
		Paths:   paths,
		Store:   store.Store{P: paths},
		Rex:     rex.Client{Bin: rexBin, Server: server},
		RexOn:   func(s string) Terminal { return rex.Client{Bin: rexBin, Server: s} },
		Crews:   crew.Load,
		AmqBin:  amqBin,
		SelfBin: self,
		Now:     time.Now,
	}, nil
}

// EnvCommand is what agents run to load their identity.
func (e Env) EnvCommand() string { return e.SelfBin + " env" }

// HookInput is the part of a hook payload the adapter reads. Claude Code and
// Codex use the snake_case names; Antigravity uses camelCase and different
// fields (see normalize).
type HookInput struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
	// Codex internal memory sessions are ephemeral and explicitly send null.
	// Keep raw JSON to distinguish that from older hooks omitting the field.
	TranscriptPath json.RawMessage `json:"transcript_path"`
	TurnID         string          `json:"turn_id"`
	StopHookActive bool            `json:"stop_hook_active"`

	ConversationID    string `json:"conversationId"`
	TerminationReason string `json:"terminationReason"`

	// AgentPID is the agent's own process id, sent by an in-process
	// integration (Pi's extension); hooks run as children of a shell and
	// cannot know it, so for them the block's foreground process is used.
	AgentPID int `json:"agent_pid"`
	// Idle: the agent is at rest (sent by Pi's extension when it rescans
	// between runs), so only an announcement can start it.
	Idle bool `json:"idle"`
}

// NormalStop reports whether a Stop ends a turn normally. Antigravity also
// runs Stop when the user cancels, on errors, and at its loop limits; mail
// must not override any of those.
func (in HookInput) NormalStop() bool {
	return in.TerminationReason == "" || in.TerminationReason == "NO_TOOL_CALL"
}

// ReadHookInput decodes the payload on stdin. An empty stdin is a valid
// empty payload (a hook run by hand).
func ReadHookInput(r io.Reader) (HookInput, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return HookInput{}, err
	}
	var in HookInput
	if len(b) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return HookInput{}, fmt.Errorf("decode hook input: %w", err)
	}
	return in, nil
}

// ErrNotInRex: the caller's terminal is not a Rex block.
var ErrNotInRex = errors.New("not running in a Rex terminal (REX_BLOCK is unset)")

// Location is the Rex terminal the caller runs in.
type Location struct {
	Server  string
	Session string
	Block   string
}

// Here reads the caller's Rex location from the environment Rex sets.
func Here() (Location, error) {
	l := Location{Server: os.Getenv("REX_SERVER"), Session: os.Getenv("REX_SESSION"), Block: os.Getenv("REX_BLOCK")}
	if l.Block == "" {
		return Location{}, ErrNotInRex
	}
	return l, nil
}
