package crm

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Mutation struct {
	Kind      string `json:"kind"`
	AccountID string `json:"account_id"`
	NoteID    string `json:"note_id,omitempty"`
	Status    string `json:"status,omitempty"`
}

type account struct {
	ID               string `json:"id"`
	CustomerID       string `json:"customer_id"`
	Status           string `json:"status"`
	RepresentativeID string `json:"representative_id"`
}
type contact struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}
type note struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}
type details struct {
	account
	Contacts []contact `json:"contacts"`
	Notes    []note    `json:"notes"`
}

// Handle executes one explicitly bound CRM behavior in the caller's transaction.
func Handle(ctx context.Context, tx *sql.Tx, worldID, runID, callID, behavior string, args map[string]any) (int, any, any, error) {
	if behavior == "crm.searchAccounts" {
		query, ok := args["query"].(string)
		if !ok || strings.TrimSpace(query) == "" {
			return 400, map[string]string{"error": "invalid query"}, nil, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT a.id,a.customer_id,a.status,a.representative_id FROM crm_accounts a LEFT JOIN customers c ON c.world_id=a.world_id AND c.id=a.customer_id WHERE a.world_id=? AND instr(lower(a.id||' '||a.customer_id||' '||a.status||' '||a.representative_id||' '||COALESCE(c.name,'')),lower(?))>0 ORDER BY a.id`, worldID, strings.TrimSpace(query))
		if err != nil {
			return 0, nil, nil, err
		}
		defer rows.Close()
		accounts := []account{}
		for rows.Next() {
			var a account
			if err = rows.Scan(&a.ID, &a.CustomerID, &a.Status, &a.RepresentativeID); err != nil {
				return 0, nil, nil, err
			}
			accounts = append(accounts, a)
		}
		return 200, accounts, nil, rows.Err()
	}
	key := "account_id"
	switch behavior {
	case "crm.getAccount":
		key = "id"
	case "crm.addAccountNote", "crm.updateAccountStatus":
	default:
		return 0, nil, nil, fmt.Errorf("unknown behavior %q", behavior)
	}
	id, ok := args[key].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return 400, map[string]string{"error": "invalid " + key}, nil, nil
	}
	var body, status string
	if behavior == "crm.addAccountNote" {
		body, ok = args["body"].(string)
		if !ok || strings.TrimSpace(body) == "" {
			return 400, map[string]string{"error": "invalid body"}, nil, nil
		}
	}
	if behavior == "crm.updateAccountStatus" {
		status, ok = args["status"].(string)
		if !ok || (status != "active" && status != "needs_followup" && status != "resolved") {
			return 400, map[string]string{"error": "invalid status"}, nil, nil
		}
	}
	var a account
	err := tx.QueryRowContext(ctx, "SELECT id,customer_id,status,representative_id FROM crm_accounts WHERE world_id=? AND id=?", worldID, id).Scan(&a.ID, &a.CustomerID, &a.Status, &a.RepresentativeID)
	if err == sql.ErrNoRows {
		return 404, map[string]string{"error": "account not found"}, nil, nil
	}
	if err != nil {
		return 0, nil, nil, err
	}
	switch behavior {
	case "crm.getAccount":
		out := details{account: a, Contacts: []contact{}, Notes: []note{}}
		rows, err := tx.QueryContext(ctx, "SELECT id,name,email FROM crm_contacts WHERE world_id=? AND account_id=? ORDER BY id", worldID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		for rows.Next() {
			var c contact
			if err = rows.Scan(&c.ID, &c.Name, &c.Email); err != nil {
				rows.Close()
				return 0, nil, nil, err
			}
			out.Contacts = append(out.Contacts, c)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return 0, nil, nil, err
		}
		if err = rows.Close(); err != nil {
			return 0, nil, nil, err
		}
		rows, err = tx.QueryContext(ctx, "SELECT id,account_id,body,created_at FROM crm_notes WHERE world_id=? AND account_id=? ORDER BY created_at,id", worldID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var n note
			if err = rows.Scan(&n.ID, &n.AccountID, &n.Body, &n.CreatedAt); err != nil {
				return 0, nil, nil, err
			}
			out.Notes = append(out.Notes, n)
		}
		return 200, out, nil, rows.Err()
	case "crm.addAccountNote":
		sum := sha256.Sum256([]byte(runID + ":" + callID))
		var base string
		if err = tx.QueryRowContext(ctx, "SELECT base_at FROM worlds WHERE id=?", worldID).Scan(&base); err != nil {
			return 0, nil, nil, err
		}
		at, err := time.Parse(time.RFC3339, base)
		if err != nil {
			return 0, nil, nil, err
		}
		at = at.Add(24*time.Hour + time.Duration(binary.BigEndian.Uint32(sum[6:10])%86400)*time.Second)
		n := note{ID: "NOTE-" + hex.EncodeToString(sum[:6]), AccountID: id, Body: body, CreatedAt: at.Format(time.RFC3339)}
		if _, err = tx.ExecContext(ctx, "INSERT INTO crm_notes(world_id,id,account_id,body,created_at) VALUES(?,?,?,?,?)", worldID, n.ID, id, body, n.CreatedAt); err != nil {
			return 0, nil, nil, err
		}
		return 201, n, Mutation{Kind: "crm.note_added", AccountID: id, NoteID: n.ID}, nil
	case "crm.updateAccountStatus":
		if _, err = tx.ExecContext(ctx, "UPDATE crm_accounts SET status=? WHERE world_id=? AND id=?", status, worldID, id); err != nil {
			return 0, nil, nil, err
		}
		a.Status = status
		return 200, a, Mutation{Kind: "crm.status_updated", AccountID: id, Status: status}, nil
	}
	panic("unreachable CRM behavior")
}
