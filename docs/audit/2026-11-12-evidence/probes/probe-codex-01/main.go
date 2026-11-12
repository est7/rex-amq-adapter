package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/trust"
)

const scratch = "/tmp/rex-audit-20261112/probe-codex-01"

func must(err error) { if err != nil { panic(err) } }
func write(path string, data []byte) {
	must(os.MkdirAll(filepath.Dir(path), 0700))
	must(os.WriteFile(path, data, 0600))
}
func git(args ...string) string {
	c := exec.Command("git", args...)
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	b, err := c.CombinedOutput()
	if err != nil { panic(fmt.Sprintf("git: %v: %s", err, b)) }
	return string(b)
}

func trustProbe() {
	root, err := os.MkdirTemp(scratch, "trust-"); must(err)
	main, wt, fake, home := filepath.Join(root,"main"), filepath.Join(root,"linked"), filepath.Join(root,"unregistered"), filepath.Join(root,"home")
	git("init", "-q", main)
	git("-C", main, "worktree", "add", "--orphan", "-q", "-b", "probe", wt)
	b, err := json.Marshal(map[string]any{"trustedWorkspaces": []string{main}}); must(err)
	settings := filepath.Join(home,".gemini","antigravity-cli","settings.json")
	write(settings,b)
	must(os.MkdirAll(fake,0700))
	raw, err := os.ReadFile(filepath.Join(wt,".git")); must(err)
	write(filepath.Join(fake,".git"), raw)
	r, err := trust.Derive(home,fake); must(err)
	if r.Settings != "" { panic("control: copied pointer was trusted") }
	must(os.Remove(filepath.Join(fake,".git")))
	must(os.Symlink(filepath.Join(wt,".git"), filepath.Join(fake,".git")))
	r, err = trust.Derive(home,fake); must(err)
	registered := strings.Contains(git("-C",main,"worktree","list","--porcelain"), "worktree "+fake+"\n")
	if r.Settings == "" || registered { panic("expected unregistered symlink trust reproduction disappeared") }
	back, err := os.ReadFile(settings); must(err)
	if string(back) != string(b) { panic("trust store was unexpectedly changed") }
	fmt.Printf("TRUST: copied pointer denied; symlink pointer settings_nonempty=%v registered_in_git=%v store_unchanged=true\n", r.Settings!="", registered)
}

type terminal struct {
	app.Terminal
	blocks []rex.Block
	afterLayout func()
	launched int
}
func (t *terminal) Whoami(context.Context) (rex.Server,error) { return rex.Server{Endpoint:"unix:///probe.sock",InstanceID:"probe"},nil }
func (t *terminal) InspectBlock(context.Context,string) (rex.Block,error) { return rex.Block{ID:"block:1",SessionID:"session:probe",WindowID:"window:probe"},nil }
func (t *terminal) ListBlocks(context.Context,string) ([]rex.Block,error) { return append([]rex.Block(nil),t.blocks...),nil }
func (t *terminal) InspectSession(context.Context,string) (rex.Session,error) {
	if t.afterLayout != nil { f:=t.afterLayout; t.afterLayout=nil; f() }
	return rex.Session{ID:"session:probe"},nil
}
func (t *terminal) Size(context.Context,string) (rex.Size,error) { return rex.Size{Rows:40,Columns:120},nil }
func (t *terminal) NewSplit(context.Context,string,string,string,string,[]string) (string,error) { t.launched++; return "block:seventh",nil }

func capProbe(race bool) {
	root, err := os.MkdirTemp(scratch,"cap-"); must(err)
	p := store.Paths{Home:filepath.Join(root,"state")}
	s := store.Store{P:p}
	t := &terminal{}
	e := app.Env{Paths:p,Store:s,Rex:t,Now:time.Now,TranscriptHome:filepath.Join(root,"home")}
	put := func(i int) {
		must(s.PutRecord(store.Record{Handle:core.Handle(fmt.Sprintf("claude-%02d",i)),Kind:core.Claude,BlockID:fmt.Sprintf("block:%d",i),Server:"unix:///probe.sock",SessionID:"session:probe",WindowID:"window:probe",Role:"worker",Cwd:root,AdoptedAt:time.Now()}))
	}
	for i:=1;i<=6;i++ {
		if race && i==6 { continue }
		put(i)
		t.blocks=append(t.blocks,rex.Block{ID:fmt.Sprintf("block:%d",i),WindowID:"window:probe",SessionID:"session:probe"})
	}
	if race {
		must(s.PutSummon("in-flight",store.Summon{Parent:"claude-01",Depth:1,For:"editor",Session:"session:probe",At:time.Now()}))
		t.afterLayout=func(){
			unlock,err:=s.LockLifecycle(context.Background());must(err);defer unlock()
			_,ok,err:=s.TakeSummon("in-flight");must(err);if !ok { panic("missing reservation") }
			put(6)
		}
	}
	r,err:=e.Summon(context.Background(),app.Location{Block:"block:1"},app.SummonRequest{Target:"reviewer",Kind:"review_request",Subject:"probe",Body:"probe",Asked:"fixture authorization"});must(err)
	recs,err:=s.Records();must(err)
	pending,err:=s.PendingSummons("session:probe",time.Now());must(err)
	if !race && r.Action!="refused" { panic("control: six visible agents did not refuse") }
	if race && (r.Action!="start" || len(recs)+pending!=7) { panic(fmt.Sprintf("expected stale-layout cap reproduction: %+v live=%d pending=%d",r,len(recs),pending)) }
	fmt.Printf("CAP: adoption_after_layout=%v action=%s live_records=%d pending=%d launched=%d\n",race,r.Action,len(recs),pending,t.launched)
}

func main() {
	if len(os.Args)>1 && os.Args[1]=="round2b" {round2bProbe();return}
	if len(os.Args)>1 && os.Args[1]=="round2" {round2Probe();return}
	if len(os.Args)>1 && os.Args[1]=="hook" { hookProbe(false);return }
	if len(os.Args)>1 && os.Args[1]=="profile" { hookProbe(true);return }
	trustProbe(); capProbe(false); capProbe(true)
}
