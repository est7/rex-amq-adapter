package main
import (
 "encoding/json"
 "fmt"
 "os"
 "strings"
 "github.com/est7/rex-amq-adapter/internal/screen"
 "github.com/est7/rex-amq-adapter/internal/core"
)
func main() {
 cases := []struct{name,top,box string}{
  {"wide titled empty","────────── qa(claude-01) | [audit] task ─","❯ "},
  {"narrow titled empty"," implementer(claude-90) | Track B Slice s1: CanExch… ─","❯ "},
  {"narrow titled draft"," implementer(claude-90) | Track B Slice s1: CanExch… ─","❯ existing draft"},
  {"short titled empty","─ qa(claude-01) | [audit] task ─","❯ "},
 }
 enc:=json.NewEncoder(os.Stdout)
 for _,c:=range cases { input:=c.top+"\n"+c.box+"\n"+strings.Repeat("─",54)+"\n"; enc.Encode(map[string]string{"name":c.name,"screen":input,"actual":screen.Check("claude",input).String()}) }
 for _,s:=range []string{strings.Repeat("修",17),strings.Repeat("🚀",17),strings.Repeat("👩‍💻",8)} { fmt.Printf("Task(%q)=%q\n",s,core.Task(s)) }
}
