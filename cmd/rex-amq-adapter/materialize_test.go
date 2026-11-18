package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/app"
)

// Writing into an existing directory is a non-atomic batch, file by file,
// so its envelope is meta.complete=false whenever it wrote (spec 4.3(8),
// codex-08 Slice 3 R1 S4); a new directory created whole, a dry run, and a
// run that changed nothing are not marked, and a batch that stopped part
// way is marked with its items.
func TestMaterializeEnvelopeMarksTheBatch(t *testing.T) {
	failed := errors.New("part way")
	for _, c := range []struct {
		name       string
		res        app.MaterializeResult
		err        error
		incomplete bool
		data       bool
	}{
		{"into an existing directory", app.MaterializeResult{Changed: true, Complete: true, Items: []app.MaterializedItem{{Status: app.MatReplaced}}}, nil, true, true},
		{"a new directory, whole", app.MaterializeResult{Atomic: true, Changed: true, Complete: true}, nil, false, true},
		{"nothing to change", app.MaterializeResult{Complete: true}, nil, false, true},
		{"a dry run", app.MaterializeResult{Changed: true, Complete: true, DryRun: true}, nil, false, true},
		{"stopped part way", app.MaterializeResult{Changed: true, Items: []app.MaterializedItem{{Status: app.MatFailed}}}, failed, true, true},
		{"refused before any item", app.MaterializeResult{}, failed, false, false},
	} {
		r, err := materializeResult(c.res, c.err)
		if !errors.Is(err, c.err) || r.Incomplete != c.incomplete || (r.Data != nil) != c.data {
			t.Errorf("%s: incomplete=%v data=%v err=%v; want incomplete=%v", c.name, r.Incomplete, r.Data != nil, err, c.incomplete)
		}
	}
}

// epic show is read by the Epic's lead (whichever role leads it), its
// narrator, its personal agent, and the user outside Rex.
func TestEpicShowRoles(t *testing.T) {
	c := registry().Find("epic", "show")
	if c == nil || !slices.Equal(c.Roles, []string{"lead", "narrator", "personal", "human"}) {
		t.Fatalf("epic show roles: %+v", c)
	}
}
