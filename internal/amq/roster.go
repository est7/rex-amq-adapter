package amq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// A handle that will never be resumed (its Epic closed, or it was retired as
// finished) leaves the root's roster: every `amq init --force` rewrites the
// roster and fsyncs each listed mailbox's directories, so a roster that only
// grows makes every new mailbox slower to make. amq stays the single owner of
// its queue layout and of meta/config.json: the adapter removes a mailbox with
// `amq cleanup --mailbox` and rewrites the roster with `amq init --force`.

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

// PruneAgents takes the handles of drop out of root's roster, under the
// registry lock that every roster writer holds, by rewriting it with `amq
// init --force` and the agents that remain. It returns the handles it took
// out. A roster that would be left empty is left as it is: amq needs at
// least one agent, and a roster of only unresumable handles costs nothing.
func PruneAgents(ctx context.Context, amqBin, root string, drop []string) ([]string, error) {
	unlock, err := lockRegistry(ctx, root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	agents, err := Agents(root)
	if err != nil {
		return nil, err
	}
	keep := slices.DeleteFunc(slices.Clone(agents), func(a string) bool { return slices.Contains(drop, a) })
	pruned := slices.DeleteFunc(slices.Clone(agents), func(a string) bool { return !slices.Contains(drop, a) })
	if len(pruned) == 0 || len(keep) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, amqBin, "--no-update-check", "init", "--root", root, "--agents", strings.Join(keep, ","), "--force")
	cmd.Env = amqEnv()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("amq init --force %s: %w: %s", root, err, strings.TrimSpace(out.String()))
	}
	return pruned, nil
}
