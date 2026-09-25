package dispatch

import (
 "context"
 "encoding/json"
 "os"
 "path/filepath"
 "testing"

 "twinwright/internal/compiler"
 "twinwright/internal/store"
)

func setup(t *testing.T, fault string) (*Dispatcher,*store.Store,store.Run) {
 t.Helper(); ctx:=context.Background()
 root:=filepath.Join("..","..","examples","billing")
 spec,err:=os.ReadFile(filepath.Join(root,"openapi.yaml"));if err!=nil{t.Fatal(err)}
 bindings,err:=os.ReadFile(filepath.Join(root,"bindings.yaml"));if err!=nil{t.Fatal(err)}
 manifest,err:=compiler.Compile(spec,bindings);if err!=nil{t.Fatal(err)}
 s,err:=store.Open(t.TempDir()+"/world.db");if err!=nil{t.Fatal(err)};t.Cleanup(func(){s.Close()})
 w,err:=s.Seed(ctx,42,manifest.Digest);if err!=nil{t.Fatal(err)}
 run,err:=s.CreateRun(ctx,w.ID,"duplicate-charge","scripted","task",fault);if err!=nil{t.Fatal(err)}
 return &Dispatcher{Store:s,Manifest:manifest},s,run
}

func TestRefundMutatesStateAndRepeatedCallIsSafe(t *testing.T){
 d,s,r:=setup(t,"");ctx:=context.Background()
 before,err:=d.Invoke(ctx,r.ID,"get-1","getCharge",map[string]any{"id":"CH-1002"});if err!=nil{t.Fatal(err)}
 var charge struct{AmountCents int64 `json:"amount_cents"`;RefundedCents int64 `json:"refunded_cents"`}
 if err=json.Unmarshal(before.Body,&charge);err!=nil{t.Fatal(err)}
 if before.Status!=200||charge.RefundedCents!=0{t.Fatalf("before=%+v %+v",before,charge)}
 args:=map[string]any{"charge_id":"CH-1002","amount_cents":charge.AmountCents,"reason":"duplicate charge"}
 first,err:=d.Invoke(ctx,r.ID,"refund-1","createRefund",args);if err!=nil{t.Fatal(err)}
 second,err:=d.Invoke(ctx,r.ID,"refund-1","createRefund",args);if err!=nil{t.Fatal(err)}
 if first.Status!=201||string(first.Body)!=string(second.Body){t.Fatalf("responses: %+v %+v",first,second)}
 after,err:=d.Invoke(ctx,r.ID,"get-2","getCharge",map[string]any{"id":"CH-1002"});if err!=nil{t.Fatal(err)}
 if err=json.Unmarshal(after.Body,&charge);err!=nil{t.Fatal(err)}
 if charge.RefundedCents!=charge.AmountCents{t.Fatalf("charge after refund=%+v",charge)}
 var count int;if err=s.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?",r.WorldID).Scan(&count);err!=nil{t.Fatal(err)}
 if count!=1{t.Fatalf("refund count=%d",count)}
}

func TestOneShotFaultThenNewCallSucceeds(t *testing.T){
 d,_,r:=setup(t,"listCharges");ctx:=context.Background();args:=map[string]any{"id":"INV-104"}
 a,err:=d.Invoke(ctx,r.ID,"call-a","listCharges",args);if err!=nil{t.Fatal(err)}
 repeat,err:=d.Invoke(ctx,r.ID,"call-a","listCharges",args);if err!=nil{t.Fatal(err)}
 b,err:=d.Invoke(ctx,r.ID,"call-b","listCharges",args);if err!=nil{t.Fatal(err)}
 if a.Status!=503||repeat.Status!=503||b.Status!=200{t.Fatalf("statuses=%d,%d,%d",a.Status,repeat.Status,b.Status)}
}
