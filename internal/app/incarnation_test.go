package app

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// The Linux reader counts fields after the last ')', so a comm holding
// spaces and parentheses does not shift starttime (field 22).
func TestIncarnationProcStatReadsStartTimeAfterComm(t *testing.T) {
	line := "4242 (we) ird (name)) S 77 4242 4242 0 -1 4194560 1 0 0 0 3 4 0 0 20 0 1 0 987654 1000 200 18446744073709551615\n"
	st, err := parseProcStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "we) ird (name)" || st.PPID != 77 || st.StartTime != 987654 {
		t.Fatalf("parsed %+v", st)
	}
	for _, bad := range []string{"4242 no comm", "4242 (x) S 1 2 3", "4242 (x) S ppid 4242 4242 0 -1 4194560 1 0 0 0 3 4 0 0 20 0 1 0 9"} {
		if _, err := parseProcStat(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// The start time tells a process from a later one: it is stable for one
// process and differs for another that started after it.
func TestIncarnationStartTimeOfARealProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no start time reader on " + runtime.GOOS)
	}
	self, err := processStartTime(os.Getpid())
	if err != nil || self <= 0 {
		t.Fatalf("this process: %d %v", self, err)
	}
	if again, _ := processStartTime(os.Getpid()); again != self {
		t.Fatalf("one process read %d then %d", self, again)
	}
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	other, err := processStartTime(child.Process.Pid)
	if err != nil || other == self {
		t.Fatalf("a later process: %d (%v), this one %d", other, err, self)
	}
	if _, err := processStartTime(-1); err == nil {
		t.Fatal("no process has pid -1")
	}
}
