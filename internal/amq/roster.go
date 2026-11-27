package amq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// A handle that will never be resumed (its Epic closed, or it was retired as
// finished) loses its mailbox: the adapter removes it with `amq cleanup
// --mailbox`, and amq stays the single owner of its queue layout and of
// meta/config.json. The handle's roster entry stays: `amq init --add` checks
// only the handles it is asked for, so a longer roster costs a new mailbox
// nothing, and the roster is bounded by the peak number of agents that ran at
// once because core.NameHandle reuses the lowest free name. A send to such a
// handle is refused before amq runs (core.CanExchange), since it has no
// identity.

// MailboxRemoval is what `amq cleanup --mailbox` did with one handle's
// mailbox. Refusal is amq's reason when it kept a mailbox that holds mail
// (unread, dead letters, extension data) or that a live wake holds.
type MailboxRemoval struct {
	Removed bool   `json:"removed"`
	Absent  bool   `json:"absent"`
	Refusal string `json:"refusal"`
}

// Gone reports whether the handle has no mailbox now.
func (r MailboxRemoval) Gone() bool { return r.Removed || r.Absent }

// RemoveMailbox removes handle's mailbox under root with amq's own checks: a
// mailbox holding mail, or held by a live wake, is kept and its Refusal
// says why. Removing an absent mailbox is not an error.
func RemoveMailbox(ctx context.Context, amqBin, root, handle string) (MailboxRemoval, error) {
	cmd := exec.CommandContext(ctx, amqBin, "--no-update-check", "cleanup", "--mailbox", handle, "--root", root, "--yes", "--json")
	cmd.Env = amqEnv()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	var r MailboxRemoval
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		if runErr != nil {
			return r, fmt.Errorf("amq cleanup --mailbox %s: %w: %s", handle, runErr, strings.TrimSpace(errOut.String()))
		}
		return r, fmt.Errorf("amq cleanup --mailbox %s: decode %q: %w", handle, out.String(), err)
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit) && exit.ExitCode() == 6 && r.Refusal != "":
		// Action required: amq kept the mailbox and says why.
	default:
		return r, fmt.Errorf("amq cleanup --mailbox %s: %w: %s", handle, runErr, strings.TrimSpace(errOut.String()))
	}
	return r, nil
}
