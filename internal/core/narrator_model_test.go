package core

import "testing"

// A narrator runs on its CLI's cheap tier; every other role, and a CLI with
// no cheap tier named, keeps the CLI's own default.
func TestNarratorRunsOnTheCheapModel(t *testing.T) {
	for _, tc := range []struct {
		role string
		kind Kind
		want string
	}{
		{NarratorRole, Claude, "haiku"},
		{NarratorRole, Agy, "gemini-4-hydrogen-low"},
		{NarratorRole, Codex, ""},
		{NarratorRole, Pi, ""},
		{OrchestratorRole, Claude, ""},
		{"implementer", Agy, ""},
		{PersonalRole, Claude, ""},
	} {
		if got := RoleModel(tc.role, tc.kind); got != tc.want {
			t.Errorf("RoleModel(%q, %s) = %q, want %q", tc.role, tc.kind, got, tc.want)
		}
	}
}
