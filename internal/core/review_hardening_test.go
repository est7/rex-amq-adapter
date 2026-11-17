package core

import "testing"

// A lease's process that is gone is gone whatever runs in its block now: a
// reused pid that happens to be the block's foreground is another program,
// and a record with no known process is never the foreground.
func TestCheckBindingPIDReuseIsNotTheAgent(t *testing.T) {
	for _, c := range []struct {
		name string
		b    Binding
		ok   bool
		want Progress
		code string
	}{
		{"the gone agent's pid reused by the foreground", Binding{ForegroundPID: 9, RecordPID: 9, AgentGone: true}, false, Failed, "agent_gone"},
		{"no foreground and no known process", Binding{}, false, Deferred, "agent_not_foreground"},
		{"the live agent is the foreground", Binding{ForegroundPID: 9, RecordPID: 9}, true, "", ""},
	} {
		o, ok := CheckBinding(c.b)
		if ok != c.ok || o.Progress != c.want || o.Code != c.code {
			t.Errorf("%s: %+v: got %+v ok=%v, want %s %s", c.name, c.b, o, ok, c.want, c.code)
		}
	}
}
