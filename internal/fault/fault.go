// Package fault is the closed set of error codes every public command can
// fail with, the process exit status each maps to, and the typed error that
// carries one through the program. Agents branch on Code, so a new value is
// a contract change: add it to codes below, with its exit status, and never
// renumber one (`rex-amq-adapter schema errors` publishes the table).
package fault

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Code is a closed enum of error codes.
type Code string

const (
	// Exit 1: a runtime or upstream failure.
	Internal       Code = "internal"
	RexUnavailable Code = "rex_unavailable"
	AMQUnavailable Code = "amq_unavailable"
	Timeout        Code = "timeout"
	ResourceLocked Code = "resource_locked"
	// Exit 2: the call is wrong; never retry it unchanged.
	Usage                Code = "usage"
	InvalidInput         Code = "invalid_input"
	ConfirmationRequired Code = "confirmation_required"
	// Exit 3: a gateway or policy refusal, or no identity to act as.
	PermissionDenied Code = "permission_denied"
	UntrustedFolder  Code = "untrusted_folder"
	UnapprovedHooks  Code = "unapproved_hooks"
	NoIdentity       Code = "no_identity"
	// Exit 4: the named thing does not exist (details.kind says which).
	NotFound Code = "not_found"
	// Exit 5: the state forbids the change.
	Conflict           Code = "conflict"
	Busy               Code = "busy"
	CapReached         Code = "cap_reached"
	FailedPrecondition Code = "failed_precondition"
	// Exit 6: a guard refused the caller (details.reason says which).
	GuardDenied Code = "guard_denied"
	// Exit 7: part of the change took effect; data holds what did.
	PartialStart Code = "partial_start"
	NotAdopted   Code = "not_adopted"
	SeatFailed   Code = "seat_failed"
	// Signals, after cleanup.
	Interrupted Code = "interrupted"
	Terminated  Code = "terminated"
)

// Info is one row of the published table.
type Info struct {
	Code        Code   `json:"code"`
	Exit        int    `json:"exit"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description"`
}

// codes is the table, in exit order: the one owner of each code's exit
// status, default retryability, and description.
var codes = []Info{
	{Internal, 1, false, "unexpected failure inside rex-amq-adapter; a crash dump is in <state>/logs"},
	{RexUnavailable, 1, true, "the Rex server or the rex binary cannot be reached"},
	{AMQUnavailable, 1, true, "the amq binary is missing or failed to run"},
	{Timeout, 1, true, "a bounded wait ran out"},
	{ResourceLocked, 1, true, "another adapter process holds a lock; retry after details.retry_after_ms"},
	{Usage, 2, false, "unknown command, flag, or a missing or malformed argument; fix the call"},
	{InvalidInput, 2, false, "a value failed validation before any I/O (ids, paths, text kinds)"},
	{ConfirmationRequired, 2, false, "a named confirmation is missing (--asked, --confirm purge)"},
	{PermissionDenied, 3, false, "the gateway or a policy refused; details.reason names the rule"},
	{UntrustedFolder, 3, false, "the directory is not trusted; the user runs `rex-amq-adapter trust grant <dir>`"},
	{UnapprovedHooks, 3, false, "Codex has not approved the adapter's hooks; the user approves them with /hooks"},
	{NoIdentity, 3, false, "the caller has no live AMQ identity in this pane"},
	{NotFound, 4, false, "the named item does not exist; details.kind is agent, session, block, crew, role, message, thread, artifact, topic, or hub"},
	{Conflict, 5, false, "an id reused for another request, or a target in a state that forbids the change"},
	{Busy, 5, true, "an agent is not at rest or holds unread mail; wait, or (only when the user says so) force"},
	{CapReached, 5, false, "the session already runs its maximum number of agents"},
	{FailedPrecondition, 5, false, "a precondition does not hold (details.reason, such as not_in_rex)"},
	{GuardDenied, 6, false, "a guard refused the caller; details.reason is human_only, unattended, personal_role_locked, personal_session_pure, or epic_session_no_personal"},
	{PartialStart, 7, false, "part of the start took effect; data holds what did"},
	{NotAdopted, 7, false, "an agent was launched but not adopted; data holds its startup"},
	{SeatFailed, 7, false, "a council seat could not be seated; data holds each seat"},
	{Interrupted, 130, false, "SIGINT, after cleanup"},
	{Terminated, 143, false, "SIGTERM, after cleanup"},
}

// Table is the published table, in exit order.
func Table() []Info { return append([]Info(nil), codes...) }

func info(c Code) (Info, bool) {
	for _, i := range codes {
		if i.Code == c {
			return i, true
		}
	}
	return Info{}, false
}

// ExitCode is c's process exit status; an unknown code is internal's.
func ExitCode(c Code) int {
	if i, ok := info(c); ok {
		return i.Exit
	}
	return 1
}

// Known reports whether c is in the closed set.
func Known(c Code) bool { _, ok := info(c); return ok }

// Error is a failure with its code. It wraps an optional cause, so an error
// a package already returns (app.ErrRefused) keeps matching errors.Is.
type Error struct {
	Code       Code
	Message    string
	Hint       string
	NextAction string
	Details    map[string]any
	// retryable overrides the code's default when set.
	retryable *bool
	cause     error
}

func (e *Error) Error() string { return e.Message }

// Unwrap is the cause, nil when there is none.
func (e *Error) Unwrap() error { return e.cause }

// New is an error with code c.
func New(c Code, format string, a ...any) *Error {
	return &Error{Code: c, Message: fmt.Sprintf(format, a...)}
}

// Wrap is an error with code c whose message is cause's.
func Wrap(cause error, c Code) *Error {
	return &Error{Code: c, Message: cause.Error(), cause: cause}
}

// WithCause sets the error errors.Is and errors.As see through e.
func (e *Error) WithCause(cause error) *Error { e.cause = cause; return e }

// With sets one key of details.
func (e *Error) With(key string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = v
	return e
}

// WithHint sets the human hint.
func (e *Error) WithHint(h string) *Error { e.Hint = h; return e }

// WithNext sets the exact command that fixes the failure.
func (e *Error) WithNext(cmd string) *Error { e.NextAction = cmd; return e }

// WithRetryable overrides the code's default retryability.
func (e *Error) WithRetryable(r bool) *Error { e.retryable = &r; return e }

// Retryable reports whether an unchanged retry may succeed.
func (e *Error) Retryable() bool {
	if e.retryable != nil {
		return *e.retryable
	}
	i, _ := info(e.Code)
	return i.Retryable
}

// Exit is e's process exit status.
func (e *Error) Exit() int { return ExitCode(e.Code) }

// MarshalJSON is the envelope's error object: every key always present,
// next_action null when there is none, details an object.
func (e *Error) MarshalJSON() ([]byte, error) {
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	var next *string
	if e.NextAction != "" {
		next = &e.NextAction
	}
	return json.Marshal(struct {
		Code       Code           `json:"code"`
		Message    string         `json:"message"`
		Retryable  bool           `json:"retryable"`
		Hint       string         `json:"hint"`
		NextAction *string        `json:"next_action"`
		Details    map[string]any `json:"details"`
	}{e.Code, e.Message, e.Retryable(), e.Hint, next, details})
}

// As is the *Error in err's chain, if any.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
