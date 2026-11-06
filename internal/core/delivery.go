package core

import (
	"fmt"
	"time"
)

// Attempt is one injector outcome and when it happened.
type Attempt struct {
	At time.Time `json:"at"`
	Outcome
}

// LastDelivery describes an agent's doorbell history for a person deciding
// whether delivery is stuck: the latest outcome, and for how long the same
// outcome has repeated. "deferred box_not_found 23m" means every doorbell for
// 23 minutes found no input box, which no other signal reveals; a few seconds
// of agent_working is ordinary. history is oldest first.
func LastDelivery(history []Attempt, now time.Time) string {
	last, since, ok := lastRun(history)
	if !ok {
		return "-"
	}
	return summarize(&DoorbellState{Progress: last.Progress, Code: last.Code, Since: since}, 1, now)
}

// DeliveryStatus is LastDelivery for an agent holding unread mail. An outcome
// short of accepted explains why a doorbell did not ring; with nothing unread
// none is due, so that outcome is history and no sign of a stuck delivery.
func DeliveryStatus(history []Attempt, unread int, now time.Time) string {
	last, since, ok := lastRun(history)
	if !ok {
		return "-"
	}
	return summarize(&DoorbellState{Progress: last.Progress, Code: last.Code, Since: since}, unread, now)
}

// Ago is a compact duration: 40s, 23m, 5h, 2d.
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
