package main

import (
	"bytes"
	"context"
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

func flagsWorld() app.Env {
	e:=amqFixture("flags-")
	for _,r:=range []store.Record{
		{Handle:"claude-01",Kind:core.Claude,Role:"worker",BlockID:"block:caller",Server:"unix:///probe.sock",SessionID:"session:a"},
		{Handle:"codex-01",Kind:core.Codex,Role:"worker",BlockID:"block:local",Server:"unix:///probe.sock",SessionID:"session:a"},
		{Handle:"claude-02",Kind:core.Claude,Role:"worker",BlockID:"block:impersonated",Server:"unix:///probe.sock",SessionID:"session:b"},
		{Handle:"codex-02",Kind:core.Codex,Role:"worker",BlockID:"block:foreign",Server:"unix:///probe.sock",SessionID:"session:b"},
	} {must(e.Store.PutRecord(r));must(amq.EnsureMailbox(context.Background(),e.AmqBin,e.Paths.Root(),string(r.Handle)))}
	return e
}

func managedCapture(e app.Env,block,verb string,args ...string)(int,string,error){
	var out,errb bytes.Buffer
	code,err:=e.ManagedMail(context.Background(),app.Location{Block:block},verb,args,nil,&out,&errb)
	return code,out.String()+errb.String(),err
}

func flagProbe(){
	e:=flagsWorld();ctx:=context.Background()
	code,_,err:=managedCapture(e,"block:caller","send","--to","codex-02","--body","control","--subject","normal")
	if err==nil {panic("control: direct cross-session send was not refused")}
	fmt.Printf("FLAGS: plain_cross_epic_send code=%d refused=true\n",code)
	marker:="isolated-routing-override"
	code,_,err=managedCapture(e,"block:caller","send","--to","codex-01","--body",marker,"--subject","--","--me","claude-02","--to","codex-02","--context","{}","--json=false")
	if err==nil {panic("expected child text output to fail JSON ID extraction")}
	ids,rerr:=amq.Unread(e.Paths.Root(),"codex-02");must(rerr)
	if len(ids)!=1 {panic(fmt.Sprintf("expected one misrouted send, got %v",ids))}
	h,rerr:=amq.ReadHeader(e.Paths.Root(),"codex-02",amq.BoxUnread,ids[0]);must(rerr)
	_,prov,rerr:=e.Store.ProvenanceOf(h.ID,"",core.Handle(h.From),h.To);must(rerr)
	readCode,out,readErr:=managedCapture(e,"block:foreign","drain","--json","--include-body");must(readErr)
	if readCode!=0 || !strings.Contains(out,marker) || h.From!="claude-02" || prov {panic("send override was not admitted as the impersonated sender")}
	fmt.Printf("FLAGS: send_hidden_override code=%d returned_error=true delivered_from=%s delivered_to=%v provenance=%v foreign_managed_drain_exposed=true\n",code,h.From,h.To,prov)

	// Both reply IDs are genuine AMQ messages, in separate sessions.
	must(amq.Send(ctx,e.AmqBin,e.Paths.Root(),"codex-01","claude-01","local request","local allowed body"))
	localIDs,rerr:=amq.Unread(e.Paths.Root(),"claude-01");must(rerr)
	must(amq.Send(ctx,e.AmqBin,e.Paths.Root(),"codex-02","claude-02","foreign request","foreign protected body"))
	foreignIDs,rerr:=amq.Unread(e.Paths.Root(),"claude-02");must(rerr)
	code,_,err=managedCapture(e,"block:caller","reply","--id",localIDs[0],"--body","isolated-reply-override","--subject","--","--me","claude-02","--id",foreignIDs[0],"--context","{}","--json=false")
	if err==nil {panic("expected reply output ID extraction failure")}
	readCode,out,readErr=managedCapture(e,"block:foreign","drain","--json","--include-body");must(readErr)
	if readCode!=0 || !strings.Contains(out,"isolated-reply-override") {panic("reply override was not delivered")}
	fmt.Printf("FLAGS: reply_hidden_override code=%d returned_error=true foreign_managed_drain_exposed=true\n",code)

	// Read has no non-routing string flag left after --id extraction. Test
	// the same hiding mechanism and a literal delimiter against real AMQ.
	readAttempts:=[][]string{
		{"--id",localIDs[0],"--id","--","--me","claude-02","--id",foreignIDs[0],"--json"},
		{"--id",localIDs[0],"--","--me","claude-02","--id",foreignIDs[0],"--json"},
	}
	for i,a:=range readAttempts {
		code,out,err=managedCapture(e,"block:caller","read",a...)
		if strings.Contains(out,"foreign protected body") {panic("read unexpectedly disclosed foreign protected body")}
		fmt.Printf("FLAGS: read_attempt=%d code=%d returned_error=%v foreign_body_exposed=false\n",i+1,code,err!=nil)
	}
}

type launchCapture struct {terminal; commands [][]string}
func(t *launchCapture)NewSplit(_ context.Context,_ string,_ string,_ string,_ string,command []string)(string,error){t.commands=append(t.commands,append([]string(nil),command...));return "block:launched",nil}

func launchProbe(){
	root,err:=os.MkdirTemp(scratch,"launch-flags-");must(err)
	p:=store.Paths{Home:filepath.Join(root,"state")};s:=store.Store{P:p}
	t:=&launchCapture{terminal:terminal{blocks:[]rex.Block{{ID:"block:1",SessionID:"session:probe",WindowID:"window:probe"}}}}
	e:=app.Env{Paths:p,Store:s,Rex:t,Now:time.Now,TranscriptHome:filepath.Join(root,"home")}
	must(s.PutRecord(store.Record{Handle:"claude-01",Kind:core.Claude,Role:"reviewer",Depth:1,BlockID:"block:1",Server:"unix:///probe.sock",SessionID:"session:probe",Cwd:root,AdoptedAt:time.Now()}))
	requests:=[]app.SummonRequest{
		{Target:"implementer",Kind:"todo",Harness:"codex",Body:"- review the diff",Subject:"probe",Asked:"fixture authorization",Grant:filepath.Join(root,"missing-grant")},
		{Target:"qa",Kind:"review_request",Harness:"agy",Body:"read-only fixture",Subject:"probe",Asked:"fixture authorization"},
		{Target:"architect",Kind:"question",Harness:"codex",Body:"fixture",Subject:"probe",Grant:root},
	}
	for i,r:=range requests{
		res,err:=e.Summon(context.Background(),app.Location{Block:"block:1"},r);must(err)
		if res.Action!="start" {panic(fmt.Sprintf("launch fixture refused: %+v",res))}
		cmd:=t.commands[len(t.commands)-1]
		program:=cmd[2:]
		terminator,skip:=false,false
		for _,a:=range program {if a=="--"{terminator=true};if a=="--dangerously-skip-permissions"{skip=true}}
		fmt.Printf("LAUNCH: case=%d action=%s harness=%s parent_depth=1 child_depth=%d end_of_options=%v prompt_starts_dash=%v skip_permissions=%v\n",i+1,res.Action,program[0],res.Depth,terminator,strings.HasPrefix(program[len(program)-1],"-"),skip)
	}
}

func widthProbe(){
	now:=time.Now()
	for _,cols:=range []int{39,40,50,59,60}{
		// A synthetic 50-column border. Width does not prove this is what
		// the current Claude renderer emits; only policy is exercised here.
		vt:="─ session title ─\n❯ \n"+strings.Repeat("─",40)+"\n"
		box:=screen.Check("claude",vt)
		o,_:=core.DecideWrite(core.Readiness{State:core.StateIdle,Box:box})
		history:=[]core.Attempt{{At:now.Add(-time.Minute),Outcome:o}}
		code,_,stuck:=core.Stuck(history,1,now)
		fmt.Printf("WIDTH: cols=%d rows=20 synthetic_box=%s stuck=%v code=%s may_move=%v\n",cols,box,stuck,code,core.MayMove(code,cols,20))
	}
}

func round2bProbe(){flagProbe();launchProbe();widthProbe()}
