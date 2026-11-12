package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func amqFixture(prefix string) app.Env {
	root,err:=os.MkdirTemp(scratch,prefix);must(err)
	p:=store.Paths{Home:filepath.Join(root,"state")}
	bin,err:=amq.ResolveBin();must(err)
	return app.Env{Paths:p,Store:store.Store{P:p},AmqBin:bin,Now:time.Now,TranscriptHome:filepath.Join(root,"home")}
}

func drainProbe() {
	e:=amqFixture("drain-")
	ctx:=context.Background()
	for _,r:=range []store.Record{
		{Handle:"claude-01",Kind:core.Claude,Role:"worker",BlockID:"block:worker",Server:"unix:///probe.sock",SessionID:"session:probe"},
		{Handle:"pi-01",Kind:core.Pi,Role:core.NarratorRole,BlockID:"block:narrator",Server:"unix:///probe.sock",SessionID:"session:probe"},
	} {must(e.Store.PutRecord(r));must(amq.EnsureMailbox(ctx,e.AmqBin,e.Paths.Root(),string(r.Handle)))}
	var out,errb bytes.Buffer
	code,err:=e.ManagedMail(ctx,app.Location{Block:"block:narrator"},"drain",[]string{"--include-body","--json"},nil,&out,&errb);must(err)
	if code!=0 {panic(errb.String())}
	policy:=core.CanExchange(core.Endpoint{Handle:"claude-01",Role:"worker",Server:"unix:///probe.sock",SessionID:"session:probe"},core.Endpoint{Handle:"pi-01",Role:core.NarratorRole,Server:"unix:///probe.sock",SessionID:"session:probe"},core.Operation{Kind:core.OpSend},nil)
	if policy.Allow {panic("fixture policy should deny member-to-narrator mail")}
	must(amq.SendKind(ctx,e.AmqBin,e.Paths.Root(),"claude-01","pi-01","question","probe","isolated forbidden body"))
	out.Reset();errb.Reset()
	code,err=amq.Run(ctx,e.AmqBin,nil,&out,&errb,"drain","--root",e.Paths.Root(),"--me","pi-01","--include-body","--json");must(err)
	if code!=0 || !strings.Contains(out.String(),"isolated forbidden body") {panic("raw drain did not expose fixture")}
	var raw map[string]any;must(json.Unmarshal(out.Bytes(),&raw))
	must(amq.SendKind(ctx,e.AmqBin,e.Paths.Root(),"claude-01","pi-01","question","control","second isolated forbidden body"))
	out.Reset();errb.Reset()
	code,err=e.ManagedMail(ctx,app.Location{Block:"block:narrator"},"drain",[]string{"--include-body","--json"},nil,&out,&errb);must(err)
	if code!=0 || strings.Contains(out.String(),"second isolated forbidden body") {panic("managed drain exposed forbidden mail")}
	var managed map[string]any;must(json.Unmarshal(out.Bytes(),&managed))
	q,err:=e.Store.QuarantinedIDs("pi-01");must(err)
	if len(q)!=1 {panic("managed drain did not quarantine its fixture")}
	fmt.Printf("DRAIN: policy=%s raw_count=%v raw_body_exposed=true managed_count=%v quarantined=%d\n",policy.Reason,raw["count"],managed["count"],len(q))
}

type adoptionTerm struct {
	app.Terminal
	beforeForeground func()
}
func (t *adoptionTerm) Whoami(context.Context)(rex.Server,error){return rex.Server{Endpoint:"unix:///probe.sock",InstanceID:"probe"},nil}
func (t *adoptionTerm) Foreground(context.Context,string)(rex.Process,bool,error){if t.beforeForeground!=nil {f:=t.beforeForeground;t.beforeForeground=nil;f()};return rex.Process{PID:123,Cwd:scratch},true,nil}
func (t *adoptionTerm) InspectBlock(context.Context,string)(rex.Block,error){return rex.Block{ID:"block:child",SessionID:"session:probe",WindowID:"window:probe",Label:"worker"},nil}
func (t *adoptionTerm) ListBlocks(context.Context,string)([]rex.Block,error){return []rex.Block{{ID:"block:child",WindowID:"window:probe"}},nil}
func (t *adoptionTerm) InspectSession(context.Context,string)(rex.Session,error){return rex.Session{ID:"session:probe"},nil}
func (t *adoptionTerm) RenameBlock(context.Context,string,string)error{return nil}
func (t *adoptionTerm) RenameWindow(context.Context,string,string)error{return nil}

func tokenProbe(failFirst bool) {
	e:=amqFixture("token-");ctx:=context.Background()
	must(amq.EnsureMailbox(ctx,e.AmqBin,e.Paths.Root(),"pi-01"))
	must(e.Store.PutSummon("child-token",store.Summon{Parent:"claude-01",Depth:2,For:"worker",Session:"session:probe",At:time.Now()}))
	t:=&adoptionTerm{};e.Rex=t
	records:=filepath.Join(e.Paths.Home,"records")
	if failFirst {t.beforeForeground=func(){write(records,[]byte("temporary failed-write fixture"))}}
	in:=app.HookInput{SessionID:"child-session",AgentPID:123,SummonToken:"child-token"}
	a,err:=e.Adopt(ctx,app.Location{Block:"block:child"},core.Pi,in)
	if failFirst {
		if err==nil {panic("fault injection did not fail PutRecord")}
		_,statErr:=os.Stat(filepath.Join(e.Paths.Home,"summons","child-token.json"))
		if !os.IsNotExist(statErr) {panic("token unexpectedly survived failed record write")}
		pending,perr:=e.Store.PendingSummons("session:probe",time.Now());must(perr)
		must(os.Remove(records));must(os.MkdirAll(records,0700))
		a,err=e.Adopt(ctx,app.Location{Block:"block:child"},core.Pi,in);must(err)
		if a.Record.Depth!=0 || a.Record.Parent!="" || pending!=0 {panic("expected lost lineage was not observed")}
		fmt.Printf("TOKEN: failed_PutRecord=true token_survives=false pending=%d retry_parent=%q retry_depth=%d\n",pending,a.Record.Parent,a.Record.Depth)
	} else {
		must(err)
		if a.Record.Depth!=2 || a.Record.Parent!="claude-01" {panic("control lost lineage without write failure")}
		fmt.Printf("TOKEN: failed_PutRecord=false parent=%q depth=%d\n",a.Record.Parent,a.Record.Depth)
	}
}

func narrowProbe() {
	rule:=strings.Repeat("─",40)
	for _,n:=range []int{10,9,1} {
		for _,draft:=range []bool{false,true} {
			input:="❯ "
			if draft {input+="private draft"}
			vt:=strings.Repeat("─",n)+" session title ─\n"+input+"\n"+rule+"\nstatus\n"
			box:=screen.Check("claude",vt)
			o,allowed:=core.DecideWrite(core.Readiness{State:core.StateIdle,Box:box})
			want:=screen.Unknown
			if n>=10 {want=screen.Empty;if draft {want=screen.Typed}}
			if box!=want {panic(fmt.Sprintf("screen case changed: got=%s want=%s",box,want))}
			fmt.Printf("NARROW: leading_rule=%d draft=%v box=%s write_allowed=%v reason=%s\n",n,draft,box,allowed,o.Code)
		}
	}
	box:=screen.Check("claude","── note\n❯ old prompt\n"+rule+"\n")
	if box!=screen.Unknown {panic("existing negative shape should remain unknown")}
	fmt.Println("NARROW: transcript-like short-rule control remains unknown")
}

func round2Probe(){drainProbe();tokenProbe(false);tokenProbe(true);narrowProbe()}
