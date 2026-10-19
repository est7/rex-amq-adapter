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
	if len(history) == 0 {
		return "-"
	}
	last := history[len(history)-1]
	since := last.At
	for i := len(history) - 2; i >= 0; i-- {
		a := history[i]
		if a.Progress != last.Progress || a.Code != last.Code {
			break
		}
		since = a.At
	}
	what := string(last.Progress)
	if last.Code != "" {
		what += " " + last.Code
	}
	return what + " " + Ago(now.Sub(since))
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
