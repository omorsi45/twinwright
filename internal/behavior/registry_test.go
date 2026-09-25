package behavior

import (
	"context"
	"database/sql"
	"testing"
)

func TestRegistryRejectsInvalidAndDuplicateKeys(t *testing.T) {
	r := NewRegistry()
	handler := func(context.Context, *sql.Tx, string, string, string, string, map[string]any) (int, any, any, error) {
		return 200, map[string]string{"ok": "yes"}, nil, nil
	}
	if err := r.Register("", handler); err == nil {
		t.Fatal("blank key accepted")
	}
	if err := r.Register("sample.read", nil); err == nil {
		t.Fatal("nil handler accepted")
	}
	if err := r.Register("sample.read", handler); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("sample.read", handler); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if _, ok := r.Lookup("sample.read"); !ok {
		t.Fatal("registered handler missing")
	}
	if _, ok := r.Lookup("missing.read"); ok {
		t.Fatal("unknown handler resolved")
	}
}

func TestBuiltinRegistryHasAllCompanyBehaviors(t *testing.T) {
	r := Builtin()
	for _, key := range []string{
		"billing.getCustomer", "billing.listInvoices", "billing.listCharges", "billing.getCharge", "billing.createRefund", "billing.getSubscription",
		"crm.getAccount", "crm.searchAccounts", "crm.addAccountNote", "crm.updateAccountStatus",
		"ticket.createIssue", "ticket.getIssue", "ticket.searchIssues", "ticket.addComment", "ticket.transitionIssue",
		"messaging.listChannels", "messaging.readChannel", "messaging.postMessage",
	} {
		if _, ok := r.Lookup(key); !ok {
			t.Errorf("builtin behavior %q missing", key)
		}
	}
}
