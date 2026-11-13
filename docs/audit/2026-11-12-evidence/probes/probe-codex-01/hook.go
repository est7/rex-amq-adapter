package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"time"

	"github.com/est7/rex-amq-adapter/internal/app"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

func hookCase(records, historicalSent int) app.Env {
	root,err:=os.MkdirTemp(scratch,"hook-");must(err)
	p:=store.Paths{Home:filepath.Join(root,"state")};s:=store.Store{P:p}
	for i:=1;i<=records;i++ {
		r:=store.Record{Handle:core.Handle(fmt.Sprintf("codex-%05d",i)),Kind:core.Codex,BlockID:fmt.Sprintf("block:%d",i),Server:"ssh://probe",SessionID:fmt.Sprintf("session:%d",i),AgentSessionID:fmt.Sprintf("thread:%d",i),AdoptedAt:time.Now()}
		// Use the real writer: all records are valid and unrelated sessions
		// are retained exactly as a busy user's state directory retains them.
		must(s.PutRecord(r))
	}
	must(s.SetSentCursor("codex-00001","999999999"))
	for i:=0;i<historicalSent;i++ {
		write(filepath.Join(p.Root(),"agents","codex-00001","outbox","sent",fmt.Sprintf("%09d.md",i)),[]byte("old history; its header is never needed after the cursor"))
	}
	return app.Env{Paths:p,Store:s,Now:time.Now}
}

func hookRun(e app.Env) {
	r,err:=e.Hook(context.Background(),app.EventPostTool,core.Codex,app.Location{Block:"block:1"},app.HookInput{SessionID:"thread:1"})
	must(err)
	if r.Notice!="" { panic("empty inbox unexpectedly announced mail") }
}

func hookProbe(profile bool) {
	cases:=[]struct{name string; records,sent int;env app.Env; samples []float64}{
		{name:"six_records_empty_outbox",records:6},
		{name:"1000_records_empty_outbox",records:1000},
		{name:"six_records_20000_old_sent",records:6,sent:20000},
	}
	for i:=range cases { cases[i].env=hookCase(cases[i].records,cases[i].sent);hookRun(cases[i].env) }
	if profile {
		f,err:=os.Create(filepath.Join(scratch,"hook.cpu"));must(err);must(pprof.StartCPUProfile(f))
		for j:=0;j<40;j++ { for _,c:=range cases { hookRun(c.env) } }
		pprof.StopCPUProfile();must(f.Close())
		fmt.Println("HOOK: profile written; timing excluded from reported samples")
		return
	}
	for j:=0;j<5;j++ {
		for i:=range cases {
			at:=time.Now()
			hookRun(cases[i].env)
			cases[i].samples=append(cases[i].samples,float64(time.Since(at).Nanoseconds())/1e6)
		}
	}
	for _,c:=range cases {
		sort.Float64s(c.samples)
		fmt.Printf("HOOK: %s n=5 median_ms=%.3f range_ms=%.3f..%.3f errors=0 notice_empty=true\n",c.name,c.samples[2],c.samples[0],c.samples[4])
	}
}
