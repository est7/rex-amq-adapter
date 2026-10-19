// Package rex is a thin client over the rex CLI. The CLI is Rex's documented,
// versioned automation surface (`rex api` is derived from the same server
// API), so the adapter talks to it rather than to the server socket.
package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrNotFound: the target does not exist (rex exit status 3).
	ErrNotFound = errors.New("rex target not found")
	// ErrUnreachable: the server could not be reached (rex exit status 4).
	ErrUnreachable = errors.New("rex server unreachable")
)

// Client runs rex against one server.
type Client struct {
	Bin    string
	Server string // REX_SERVER-style address; "" uses rex's default
}

// ResolveBin finds the rex binary. Rex installs it under a versioned
// directory that an upgrade replaces, so the path is looked up on every run
// and never stored: $REX_AMQ_REX, then PATH, then the newest bundled server.
func ResolveBin() (string, error) {
	if p := os.Getenv("REX_AMQ_REX"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("rex"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("rex not on PATH and no home directory: %w", err)
	}
	pattern := filepath.Join(home, "Library", "Application Support", "com.superlogical.rex", "RexServer", "Versions", "*", "Rex Server.app", "Contents", "MacOS", "rex")
	matches, _ := filepath.Glob(pattern) // only ErrBadPattern, impossible for a constant pattern
	if len(matches) == 0 {
		return "", errors.New("rex not found: not on PATH and no bundled Rex server; set REX_AMQ_REX")
	}
	sort.Slice(matches, func(i, j int) bool { return versionOf(matches[i]) > versionOf(matches[j]) })
	return matches[0], nil
}

// versionOf extracts the numeric Versions/<n> component; non-numeric sorts last.
func versionOf(p string) int {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, s := range parts {
		if s == "Versions" && i+1 < len(parts) {
			if n, err := strconv.Atoi(parts[i+1]); err == nil {
				return n
			}
		}
	}
	return -1
}

func (c Client) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	full := []string{"--autostart=false"}
	if c.Server != "" {
		full = append(full, "-S", c.Server)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, c.Bin, full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg := strings.TrimSpace(errb.String())
		switch ee.ExitCode() {
		case 3:
			return nil, fmt.Errorf("%w: %s", ErrNotFound, msg)
		case 4:
			return nil, fmt.Errorf("%w: %s", ErrUnreachable, msg)
		}
		return nil, fmt.Errorf("rex %s: exit %d: %s", strings.Join(args, " "), ee.ExitCode(), msg)
	}
	return nil, fmt.Errorf("rex %s: %w", strings.Join(args, " "), err)
}

func (c Client) blockCall(ctx context.Context, block, method string, args any, into any) error {
	argv := []string{"block", "call", "-b", block, method}
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		argv = append(argv, string(b))
	}
	out, err := c.run(ctx, nil, argv...)
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(out, into); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// Process is the block's foreground process.
type Process struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	Path string `json:"path"`
	Cwd  string `json:"cwd"`
}

// Foreground returns the block's foreground process. ok is false when the
// terminal reports none.
func (c Client) Foreground(ctx context.Context, block string) (p Process, ok bool, err error) {
	var res struct {
		Foreground *Process `json:"foreground"`
	}
	if err := c.blockCall(ctx, block, "process", nil, &res); err != nil {
		return Process{}, false, err
	}
	if res.Foreground == nil {
		return Process{}, false, nil
	}
	return *res.Foreground, true, nil
}

// Status is the root OSC 7501 program status record of a terminal.
type Status struct {
	State     string    `json:"state"`
	Kind      string    `json:"kind"`
	App       string    `json:"app"`
	Msg       string    `json:"msg"`
	UpdatedAt time.Time `json:"updated_at"`
	Owner     struct {
		PID  int    `json:"pid"`
		Name string `json:"name"`
	} `json:"owner"`
}

// ProgramStatus returns the root record. ok is false when there is none.
func (c Client) ProgramStatus(ctx context.Context, block string) (s Status, ok bool, err error) {
	var res struct {
		Records []struct {
			ID string `json:"id"`
			Status
		} `json:"records"`
	}
	if err := c.blockCall(ctx, block, "program_status", nil, &res); err != nil {
		return Status{}, false, err
	}
	for _, r := range res.Records {
		if r.ID == "" {
			return r.Status, true, nil
		}
	}
	return Status{}, false, nil
}

// CaptureVT returns the visible screen with its SGR styling.
func (c Client) CaptureVT(ctx context.Context, block string) (string, error) {
	out, err := c.run(ctx, nil, "capture", "-b", block, "--format=vt")
	return string(out), err
}

// Write sends bytes to the block's pty as if typed, in one write.
func (c Client) Write(ctx context.Context, block string, data string) error {
	return c.blockCall(ctx, block, "write", map[string]string{"data": data}, nil)
}

// SendKeys presses keys in Rex key syntax (enter, ctrl+u, …).
func (c Client) SendKeys(ctx context.Context, block string, keys ...string) error {
	_, err := c.run(ctx, nil, append([]string{"send-key", "-b", block}, keys...)...)
	return err
}

// Block is what the adapter needs from `rex block inspect`.
type Block struct {
	ID       string `json:"block_id"`
	Label    string `json:"label"`
	WindowID string `json:"window_id"`
}

func (c Client) InspectBlock(ctx context.Context, block string) (Block, error) {
	out, err := c.run(ctx, nil, "block", "inspect", block, "--json")
	if err != nil {
		return Block{}, err
	}
	var res struct {
		Block Block `json:"block"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return Block{}, fmt.Errorf("decode block inspect: %w", err)
	}
	return res.Block, nil
}

func (c Client) RenameBlock(ctx context.Context, block, label string) error {
	_, err := c.run(ctx, nil, "block", "rename", block, label)
	return err
}

// Server identifies the connected server process.
type Server struct {
	InstanceID string `json:"instance_id"`
	Endpoint   string `json:"endpoint"`
}

func (c Client) Whoami(ctx context.Context) (Server, error) {
	out, err := c.run(ctx, nil, "whoami", "--json")
	if err != nil {
		return Server{}, err
	}
	var s Server
	if err := json.Unmarshal(out, &s); err != nil {
		return Server{}, fmt.Errorf("decode whoami: %w", err)
	}
	if s.InstanceID == "" {
		return Server{}, errors.New("rex whoami reported no instance_id")
	}
	return s, nil
}
