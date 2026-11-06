package amq

import (
	"os"
	"testing"
)

// Tests that run amq themselves must not inherit the identity of an agent
// running them (eval "$(rex-amq-adapter env)"). TestSendIgnoresAnInheritedRoot
// sets one on purpose.
func TestMain(m *testing.M) {
	os.Unsetenv("AM_ROOT")
	os.Unsetenv("AM_ME")
	os.Exit(m.Run())
}
