// Package app runs the adapter's entry points: agent hooks, the injector amq
// wake calls, and the commands agents and users run. Policy lives in core;
// this package observes, asks core, and acts.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
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
	RenameWindow(ctx context.Context, window, label string) error
	Title(ctx context.Context, block string) (string, error)
	ListSessions(ctx context.Context) ([]string, error)
	NewSplit(ctx context.Context, session, anchor, label, dir string, command []string) (block string, err error)
	// NewSession creates a session with one window per pane, all or none
	// (Epics, internal/app/epic.go).
	NewSession(ctx context.Context, label string, panes []rex.Pane) (session string, windows, blocks []string, err error)
	// The orchestrator watchdog's layout calls (internal/app/watchdog.go).
	Size(ctx context.Context, block string) (rex.Size, error)
	DetachBlock(ctx context.Context, session, block string) error
	ResizeBlock(ctx context.Context, block string, columns, rows int) error
	NewTab(ctx context.Context, session, label string) (window, block string, err error)
	MoveBlock(ctx context.Context, session, block, anchor string) error
	CloseBlock(ctx context.Context, block string) error
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
	// TranscriptHome is the home directory the harnesses write their
	// transcripts under; Notify reaches the user's desktop (nil: macOS
	// notification center).
	TranscriptHome string
	Notify         func(title, text string)
	// Ancestors is the calling process and its ancestors, nearest first; nil
	// reads them from the OS.
	Ancestors func() ([]proc, error)
	// ClaudeDir is Claude Code's configuration directory, where its
	// supervisor lists background sessions; "" knows of none.
	ClaudeDir string
	// AgyBrainDir is Antigravity's conversation store, where a conversation
	// lists the subagents it started; "" knows of none.
	AgyBrainDir string
	// Getenv reads the harnesses' directory overrides (core.HarnessDirs);
	// nil reads none, so a test's directories stay under TranscriptHome.
	Getenv func(string) string
}

// harnessDirs is where each harness keeps its files, under TranscriptHome.
func (e Env) harnessDirs() core.Dirs { return core.HarnessDirs(e.TranscriptHome, e.Getenv) }

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
	home := userHome()
	dirs := core.HarnessDirs(home, os.Getenv)
	agyBrain := ""
	if dirs.AgyCLI != "" {
		agyBrain = filepath.Join(dirs.AgyCLI, "brain")
	}
	return Env{
		Paths:          paths,
		Store:          store.Store{P: paths},
		Rex:            rex.Client{Bin: rexBin, Server: server},
		RexOn:          func(s string) Terminal { return rex.Client{Bin: rexBin, Server: s} },
		Crews:          crew.Load,
		AmqBin:         amqBin,
		SelfBin:        self,
		Now:            time.Now,
		TranscriptHome: home,
		Getenv:         os.Getenv,
		ClaudeDir:      dirs.Claude,
		AgyBrainDir:    agyBrain,
	}, nil
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
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

	ConversationID string `json:"conversationId"`
	// Job is set when the hook's session is a Claude Code background job
	// (jobLocation): its process is not its pane's foreground.
	Job                  bool   `json:"-"`
	TerminationReason    string `json:"terminationReason"`
	Error                string `json:"error"`
	ErrorDetails         string `json:"error_details"`
	LastAssistantMessage string `json:"last_assistant_message"`
	Outcome              string `json:"outcome"`
	ErrorMessage         string `json:"error_message"`

	// AgentPID is the agent's own process id: sent by Pi's extension
	// (process.pid), and for Claude Code taken from the hook's CLAUDE_PID.
	// Other harnesses' hooks cannot know it; the block's foreground process
	// is used for them.
	AgentPID int `json:"agent_pid"`
	// SummonToken is REX_AMQ_SUMMON in the hook's environment: the token a
	// summon launched this agent with (store.Summon), binding the launch to
	// this adoption and no other.
	SummonToken string `json:"-"`
	// Idle: the agent is at rest (sent by Pi's extension when it rescans
	// between runs), so only an announcement can start it.
	Idle bool `json:"idle"`
}

// transcript is the transcript path in the payload; "" when it has none.
func (in HookInput) transcript() (string, error) {
	var path *string
	if len(in.TranscriptPath) == 0 {
		return "", nil
	}
	if err := json.Unmarshal(in.TranscriptPath, &path); err != nil {
		return "", fmt.Errorf("decode transcript_path: %w", err)
	}
	if path == nil {
		return "", nil
	}
	return *path, nil
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
