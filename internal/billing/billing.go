package billing

import (
 "context"
 "crypto/sha256"
 "database/sql"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "time"
)

type Mutation struct {RefundID string `json:"refund_id"`;ChargeID string `json:"charge_id"`;AmountCents int64 `json:"amount_cents"`}

// Handle executes one explicitly bound fictional billing behavior inside the caller's transaction.
func Handle(ctx context.Context, tx *sql.Tx, worldID, runID, callID, behavior string, args map[string]any) (int, any, *Mutation, error) {
 switch behavior {
 case "billing.getCustomer":
  var id,name string
  err:=tx.QueryRowContext(ctx,"SELECT id,name FROM customers WHERE world_id=? AND id=?",worldID,args["id"]).Scan(&id,&name)
  if err==sql.ErrNoRows{return 404,map[string]string{"error":"customer not found"},nil,nil};if err!=nil{return 0,nil,nil,err}
  return 200,map[string]any{"id":id,"name":name},nil,nil
 case "billing.listInvoices":
  rows,err:=tx.QueryContext(ctx,"SELECT id,customer_id,amount_cents,subscription_id FROM invoices WHERE world_id=? AND customer_id=? ORDER BY id",worldID,args["id"])
  if err!=nil{return 0,nil,nil,err};defer rows.Close()
  out:=[]map[string]any{}
  for rows.Next(){var id,cust,sub string;var amount int64;if err=rows.Scan(&id,&cust,&amount,&sub);err!=nil{return 0,nil,nil,err};out=append(out,map[string]any{"id":id,"customer_id":cust,"amount_cents":amount,"subscription_id":sub})}
  return 200,out,nil,rows.Err()
 case "billing.listCharges":
  rows,err:=tx.QueryContext(ctx,"SELECT id,invoice_id,amount_cents,refunded_cents,created_at FROM charges WHERE world_id=? AND invoice_id=? ORDER BY created_at",worldID,args["id"])
  if err!=nil{return 0,nil,nil,err};defer rows.Close()
  out:=[]map[string]any{}
  for rows.Next(){charge,err:=scanCharge(rows);if err!=nil{return 0,nil,nil,err};out=append(out,charge)}
  return 200,out,nil,rows.Err()
 case "billing.getCharge":
  row:=tx.QueryRowContext(ctx,"SELECT id,invoice_id,amount_cents,refunded_cents,created_at FROM charges WHERE world_id=? AND id=?",worldID,args["id"])
  charge,err:=scanCharge(row)
  if err==sql.ErrNoRows{return 404,map[string]string{"error":"charge not found"},nil,nil};if err!=nil{return 0,nil,nil,err}
  return 200,charge,nil,nil
 case "billing.createRefund":
  chargeID:=args["charge_id"].(string);amount:=integer(args["amount_cents"]);reason:=args["reason"].(string)
  if amount<=0{return 400,map[string]string{"error":"amount must be positive"},nil,nil}
  var charged,refunded int64
  err:=tx.QueryRowContext(ctx,"SELECT amount_cents,refunded_cents FROM charges WHERE world_id=? AND id=?",worldID,chargeID).Scan(&charged,&refunded)
  if err==sql.ErrNoRows{return 404,map[string]string{"error":"charge not found"},nil,nil};if err!=nil{return 0,nil,nil,err}
  if amount>charged-refunded{return 409,map[string]string{"error":"refund exceeds remaining charge"},nil,nil}
  sum:=sha256.Sum256([]byte(runID+":"+callID));refundID:="RF-"+hex.EncodeToString(sum[:6])
  var base string
  if err=tx.QueryRowContext(ctx,"SELECT base_at FROM worlds WHERE id=?",worldID).Scan(&base);err!=nil{return 0,nil,nil,err}
  baseTime,err:=time.Parse(time.RFC3339,base);if err!=nil{return 0,nil,nil,err}
  at:=baseTime.Add(24*time.Hour).Format(time.RFC3339)
  if _,err=tx.ExecContext(ctx,"INSERT INTO refunds(world_id,id,charge_id,amount_cents,reason,created_at) VALUES(?,?,?,?,?,?)",worldID,refundID,chargeID,amount,reason,at);err!=nil{return 0,nil,nil,err}
  if _,err=tx.ExecContext(ctx,"UPDATE charges SET refunded_cents=refunded_cents+? WHERE world_id=? AND id=?",amount,worldID,chargeID);err!=nil{return 0,nil,nil,err}
  return 201,map[string]any{"id":refundID,"charge_id":chargeID,"amount_cents":amount,"reason":reason,"created_at":at},&Mutation{refundID,chargeID,amount},nil
 default:
  return 0,nil,nil,fmt.Errorf("unknown behavior %q",behavior)
 }
}

type scanner interface {Scan(...any) error}
func scanCharge(row scanner)(map[string]any,error){
 var id,invoice,created string;var amount,refunded int64
 if err:=row.Scan(&id,&invoice,&amount,&refunded,&created);err!=nil{return nil,err}
 return map[string]any{"id":id,"invoice_id":invoice,"amount_cents":amount,"refunded_cents":refunded,"created_at":created},nil
}
func integer(v any) int64 {switch n:=v.(type){case int:return int64(n);case int64:return n;case float64:if n==float64(int64(n)){return int64(n)};case json.Number:x,_:=n.Int64();return x};return 0}
