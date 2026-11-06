package app

import (
	"os"
	"testing"
)

// The tests run amq themselves too; the identity of an agent running them
// (eval "$(rex-amq-adapter env)") must not stand for theirs.
func TestMain(m *testing.M) {
	os.Unsetenv("AM_ROOT")
	os.Unsetenv("AM_ME")
	os.Exit(m.Run())
}
