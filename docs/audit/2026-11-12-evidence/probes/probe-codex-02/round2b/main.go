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
 "github.com/est7/rex-amq-adapter/internal/store"
)
func must(err error) { if err!=nil { panic(err) } }
func require(ok bool,why string) { if !ok { panic(why) } }
func main(){
 ctx,cancel:=context.WithTimeout(context.Background(),40*time.Second);defer cancel()
 base:="/tmp/rex-audit-20261112/probe-codex-02/round2b"
 state,err:=os.MkdirTemp(base,"state-");must(err)
 paths:=store.Paths{Home:state}; st:=store.Store{P:paths}
 fake:=filepath.Join(state,"fake-rex")
 must(os.WriteFile(fake,[]byte(fmt.Sprintf(`#!/bin/sh
case "$*" in
 *'session inspect '*) echo '{"session_id":"session:a","label":"audit-probe","windows":[]}' ;;
 *' process') echo '{"foreground":{"pid":%d,"name":"pi"}}' ;;
 *) echo "unexpected fake Rex command: $*" >&2; exit 3 ;;
esac
`,os.Getpid())),0700))
 client:=rex.Client{Bin:fake,Server:"unix:///tmp/rex-audit-20261112/never-connect.sock"}
 e:=app.Env{Paths:paths,Store:st,Rex:client,RexOn:func(string)app.Terminal{return client},AmqBin:"/opt/homebrew/bin/amq",Now:time.Now,SelfBin:"/isolated/probe",TranscriptHome:state}
 recs:=map[string]store.Record{}
 for _,row:=range []struct{h,role,s string}{{"orch","orchestrator","a"},{"worker","implementer","a"},{"narr","narrator","a"},{"other","reviewer","b"}} {
  r:=store.Record{Handle:core.Handle(row.h),Kind:core.Pi,Role:row.role,Server:client.Server,SessionID:"session:"+row.s,BlockID:"block:"+row.h,AgentSessionID:"probe-"+row.h,AgentPID:os.Getpid(),AdoptedAt:time.Now()}
  must(st.PutRecord(r));must(amq.EnsureMailbox(ctx,e.AmqBin,paths.Root(),row.h));recs[row.h]=r
 }
 raw:=func(from,to,subject string)amq.Sent{s,_,err:=amq.Post(ctx,e.AmqBin,paths.Root(),"send","--me",from,"--to",to,"--kind","question","--subject",subject,"--body","probe body");must(err);return s}
 managed:=func(from,verb string,args ...string)(int,string,error){var out bytes.Buffer;code,err:=e.ManagedMail(ctx,app.Location{Block:"block:"+from},verb,args,strings.NewReader(""),&out,&out);return code,out.String(),err}
 result:=map[string]any{"state":state,"plan":[]string{"raw same-Epic send must leave provenance absent and narrator body reads unverified","managed same-Epic positive control must produce provenance and populate view","raw cross-Epic send must succeed then quarantine; live sender hook must not bounce","narrator control must pass pure policy only with operation/id but fail exposed managed/raw paths"}}
 s:=raw("orch","worker","raw control-plane question")
 _,has,err:=st.ProvenanceOf(s.ID,"","",nil);must(err);require(!has,"raw unexpectedly has provenance")
 v,err:=e.RefreshSessionView(ctx,app.Location{Block:"block:narr"},"");must(err)
 read,err:=e.ReadSessionMessages(ctx,app.Location{Block:"block:narr"},[]string{s.ID});must(err)
 require(len(v.Activity)==1&&len(v.ControlPlane)==0&&len(read.Messages)==0,"raw view/read differs")
 result["raw_same_epic"]=map[string]any{"id":s.ID,"provenance_exists":has,"view":v,"read":read}
 code,out,err:=managed("orch","send","--to","worker","--kind","question","--subject","managed control-plane question","--body","managed body","--json");must(err);require(code==0,"managed send exit")
 var sent amq.Sent;must(json.Unmarshal([]byte(out),&sent));_,has,err=st.ProvenanceOf(sent.ID,"","",nil);must(err);require(has,"managed provenance absent")
 v,err=e.RefreshSessionView(ctx,app.Location{Block:"block:narr"},"");must(err);require(len(v.ControlPlane)==1&&v.ControlPlane[0].Ref==sent.ID,"managed did not populate control-plane")
 read,err=e.ReadSessionMessages(ctx,app.Location{Block:"block:narr"},[]string{sent.ID});must(err);require(len(read.Messages)==1,"managed body unreadable")
 result["managed_positive_control"]=map[string]any{"exit":code,"id":sent.ID,"provenance_exists":has,"view":v,"read":read}
 code,_,err=managed("worker","drain","--include-body","--json");must(err);require(code==0,"clear prior scenario inbox")
 must(st.SetSentCursor("worker",""))
 cross:=raw("worker","other","cross-Epic raw question")
 code,out,err=managed("other","drain","--include-body","--json");must(err);require(code==0,"cross drain exit")
 qs,err:=st.QuarantinedIDs("other");must(err);require(len(qs)==1&&qs[0]==cross.ID,"cross raw not quarantined")
 q,err:=st.QuarantineReason("other",cross.ID);must(err);require(q.Decision.Reason==core.ReasonCrossEpic,"cross wrong reason")
 hook,err:=e.Hook(ctx,app.EventPostTool,core.Pi,app.Location{Block:"block:worker"},app.HookInput{SessionID:recs["worker"].AgentSessionID,AgentPID:os.Getpid()});must(err)
 require(hook.Notice=="","unexpected sender notice: "+hook.Notice)
 cursor,_,err:=st.SentCursor("worker");must(err);require(cursor==cross.ID,"sender check did not advance cursor")
 result["raw_cross_epic"]=map[string]any{"id":cross.ID,"send_success":true,"drain_exit":code,"drain_output":out,"quarantine":q,"sender_hook":hook,"sender_cursor":cursor}
 from,to:=recs["narr"].Endpoint(),recs["orch"].Endpoint()
 valid:=core.CanExchange(from,to,core.Operation{Kind:core.OpControl,ControlRequestID:"req-1"},nil);require(valid.Allow,"valid policy control denied")
 invalid:=core.CanExchange(from,to,core.Operation{Kind:core.OpControl},nil);require(!invalid.Allow,"requestless control allowed")
 code,out,err=managed("narr","send","--to","orch","--kind","todo","--subject","user requested stop","--body","stop now","--context",`{"control_request_id":"req-1","op":"control"}`)
 require(err!=nil&&strings.Contains(err.Error(),core.ReasonNarratorRoutine),"managed control was not refused")
 result["narrator_managed"]=map[string]any{"exit":code,"error":err.Error(),"stdout":out,"valid_pure_policy":valid,"missing_id_policy":invalid}
 rawcontrol:=raw("narr","orch","stop req-1")
 code,out,err=managed("orch","drain","--json");must(err);require(code==0,"orch drain failed")
 q,err=st.QuarantineReason("orch",rawcontrol.ID);must(err);require(q.Decision.Reason==core.ReasonNarratorRoutine,"raw control reason wrong")
 result["narrator_raw"]=map[string]any{"send_success":true,"id":rawcontrol.ID,"quarantine":q}
 data,err:=json.MarshalIndent(result,"","  ");must(err);must(os.WriteFile(filepath.Join(base,"results.json"),data,0600));fmt.Println(string(data))
}
