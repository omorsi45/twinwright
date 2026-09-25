package crm

import (
	"context"
	"encoding/json"
	"testing"

	"twinwright/internal/store"
)

type accountView struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Status     string `json:"status"`
	Contacts   []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"contacts"`
	Notes []noteView `json:"notes"`
}
type noteView struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

func setup(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/crm.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a, err := s.SeedScenario(context.Background(), 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.SeedScenario(context.Background(), 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	return s, a.ID, b.ID
}

func invoke(t *testing.T, s *store.Store, world, call, behavior string, args map[string]any) (int, any, any) {
	t.Helper()
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	status, body, mutation, err := Handle(context.Background(), tx, world, "run-test", call, behavior, args)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return status, body, mutation
}

func decode(t *testing.T, body, target any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func TestGetAccountReadsSeededContactsAndNotes(t *testing.T) {
	s, world, _ := setup(t)
	status, body, mutation := invoke(t, s, world, "read", "crm.getAccount", map[string]any{"id": "A-104"})
	var account accountView
	decode(t, body, &account)
	if status != 200 || mutation != nil || account.ID != "A-104" || account.CustomerID != "C-104" || len(account.Contacts) == 0 || len(account.Notes) == 0 {
		t.Fatalf("status=%d mutation=%v account=%+v", status, mutation, account)
	}
}

func TestSearchAccountsReadsCurrentRowsInStableOrder(t *testing.T) {
	s, world, _ := setup(t)
	status, body, mutation := invoke(t, s, world, "search", "crm.searchAccounts", map[string]any{"query": "a-"})
	var accounts []accountView
	decode(t, body, &accounts)
	if status != 200 || mutation != nil || len(accounts) != 2 || accounts[0].ID != "A-104" || accounts[1].ID != "A-205" {
		t.Fatalf("status=%d mutation=%v accounts=%+v", status, mutation, accounts)
	}
	_, body, _ = invoke(t, s, world, "customer-search", "crm.searchAccounts", map[string]any{"query": "Morgan"})
	decode(t, body, &accounts)
	if len(accounts) != 1 || accounts[0].ID != "A-104" {
		t.Fatalf("name matches=%+v", accounts)
	}
	_, body, _ = invoke(t, s, world, "literal-search", "crm.searchAccounts", map[string]any{"query": "%"})
	decode(t, body, &accounts)
	if len(accounts) != 0 {
		t.Fatalf("wildcard unexpectedly matched=%+v", accounts)
	}
}

func TestAccountMutationsAreVisibleAndIsolated(t *testing.T) {
	s, world, other := setup(t)
	_, beforeBody, _ := invoke(t, s, other, "before", "crm.getAccount", map[string]any{"id": "A-104"})
	var before accountView
	decode(t, beforeBody, &before)
	status, body, mutation := invoke(t, s, world, "note", "crm.addAccountNote", map[string]any{"account_id": "A-104", "body": "Duplicate charge investigated."})
	var note noteView
	decode(t, body, &note)
	if status != 201 || mutation == nil || note.ID == "" || note.AccountID != "A-104" || note.Body != "Duplicate charge investigated." || note.CreatedAt == "" {
		t.Fatalf("status=%d mutation=%v note=%+v", status, mutation, note)
	}
	status, _, mutation = invoke(t, s, world, "status", "crm.updateAccountStatus", map[string]any{"account_id": "A-104", "status": "resolved"})
	if status != 200 || mutation == nil {
		t.Fatalf("status=%d mutation=%v", status, mutation)
	}
	_, body, _ = invoke(t, s, world, "read", "crm.getAccount", map[string]any{"id": "A-104"})
	var account accountView
	decode(t, body, &account)
	found := false
	for _, n := range account.Notes {
		if n.ID == note.ID && n.Body == note.Body {
			found = true
		}
	}
	if !found || account.Status != "resolved" {
		t.Fatalf("updated account=%+v", account)
	}
	_, body, _ = invoke(t, s, other, "other", "crm.getAccount", map[string]any{"id": "A-104"})
	var unchanged accountView
	decode(t, body, &unchanged)
	if unchanged.Status != before.Status || len(unchanged.Notes) != len(before.Notes) {
		t.Fatalf("mutation leaked: before=%+v after=%+v", before, unchanged)
	}
	_, body, _ = invoke(t, s, world, "search", "crm.searchAccounts", map[string]any{"query": "resolved"})
	var accounts []accountView
	decode(t, body, &accounts)
	if len(accounts) != 1 || accounts[0].ID != "A-104" {
		t.Fatalf("stale search=%+v", accounts)
	}
}

func TestInvalidAccountRequestsDoNotMutateState(t *testing.T) {
	s, world, other := setup(t)
	if _, err := s.DB.Exec("INSERT INTO crm_accounts(world_id,id,customer_id,status,representative_id) VALUES(?,?,?,?,?)", other, "ONLY-OTHER", "C-205", "active", "REP-1"); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := s.DB.QueryRow("SELECT status FROM crm_accounts WHERE world_id=? AND id='A-104'", world).Scan(&before); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, behavior string
		args           map[string]any
		want           int
	}{
		{"unknown read", "crm.getAccount", map[string]any{"id": "missing"}, 404},
		{"other-world read", "crm.getAccount", map[string]any{"id": "ONLY-OTHER"}, 404},
		{"unknown note", "crm.addAccountNote", map[string]any{"account_id": "missing", "body": "x"}, 404},
		{"unknown status", "crm.updateAccountStatus", map[string]any{"account_id": "missing", "status": "resolved"}, 404},
		{"other-world note", "crm.addAccountNote", map[string]any{"account_id": "ONLY-OTHER", "body": "x"}, 404},
		{"invalid status", "crm.updateAccountStatus", map[string]any{"account_id": "A-104", "status": "deleted"}, 400},
		{"blank body", "crm.addAccountNote", map[string]any{"account_id": "A-104", "body": "  "}, 400},
		{"wrong type", "crm.addAccountNote", map[string]any{"account_id": 42, "body": "x"}, 400},
		{"missing id", "crm.getAccount", nil, 400},
		{"missing query", "crm.searchAccounts", nil, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, mutation := invoke(t, s, world, tc.name, tc.behavior, tc.args)
			if status != tc.want || mutation != nil {
				t.Fatalf("status=%d want=%d mutation=%v", status, tc.want, mutation)
			}
		})
	}
	var added int
	if err := s.DB.QueryRow("SELECT count(*) FROM crm_notes WHERE world_id=? AND id NOT LIKE 'SEED-%'", world).Scan(&added); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := s.DB.QueryRow("SELECT status FROM crm_accounts WHERE world_id=? AND id='A-104'", world).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if added != 0 || after != before {
		t.Fatalf("invalid request mutated state: notes=%d status=%s", added, after)
	}
}

func TestNoteIdentityAndTimestampAreDeterministic(t *testing.T) {
	s, a, b := setup(t)
	args := map[string]any{"account_id": "A-104", "body": "Resolved"}
	_, first, _ := invoke(t, s, a, "stable-call", "crm.addAccountNote", args)
	_, second, _ := invoke(t, s, b, "stable-call", "crm.addAccountNote", args)
	one, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(one) != string(two) {
		t.Fatalf("same call differs: %s vs %s", one, two)
	}
	_, third, _ := invoke(t, s, a, "other-call", "crm.addAccountNote", args)
	var x, y noteView
	decode(t, first, &x)
	decode(t, third, &y)
	if x.ID == y.ID {
		t.Fatal("different calls share a note ID")
	}
}

func TestCRMWritesParticipateInCallerRollback(t *testing.T) {
	s, world, _ := setup(t)
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	status, _, _, err := Handle(context.Background(), tx, world, "run", "rollback", "crm.addAccountNote", map[string]any{"account_id": "A-104", "body": "Rolled back"})
	if err != nil || status != 201 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM crm_notes WHERE world_id=? AND body='Rolled back'", world).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("note escaped rollback")
	}
}
