package behavior

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"twinwright/internal/billing"
	"twinwright/internal/crm"
	"twinwright/internal/messaging"
	"twinwright/internal/ticketing"
)

// Handler runs one bound behavior inside the dispatcher's transaction.
type Handler func(context.Context, *sql.Tx, string, string, string, string, map[string]any) (int, any, any, error)

type Registry struct {
	handlers map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Register adds one fully qualified behavior before the registry is used.
func (r *Registry) Register(key string, handler Handler) error {
	parts := strings.Split(key, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || handler == nil {
		return fmt.Errorf("invalid behavior registration %q", key)
	}
	if _, exists := r.handlers[key]; exists {
		return fmt.Errorf("duplicate behavior registration %q", key)
	}
	r.handlers[key] = handler
	return nil
}

func (r *Registry) Lookup(key string) (Handler, bool) {
	handler, ok := r.handlers[key]
	return handler, ok
}

func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.handlers))
	for key := range r.handlers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func billingHandler(ctx context.Context, tx *sql.Tx, worldID, runID, callID, key string, args map[string]any) (int, any, any, error) {
	status, body, mutation, err := billing.Handle(ctx, tx, worldID, runID, callID, key, args)
	if mutation == nil {
		return status, body, nil, err
	}
	return status, body, mutation, err
}

// Builtin returns the four local service modules. Callers may register more
// handlers on the returned registry before using it.
func Builtin() *Registry {
	r := NewRegistry()
	modules := []struct {
		keys    []string
		handler Handler
	}{
		{[]string{"billing.getCustomer", "billing.listInvoices", "billing.listCharges", "billing.getCharge", "billing.createRefund", "billing.getSubscription"}, billingHandler},
		{[]string{"crm.getAccount", "crm.searchAccounts", "crm.addAccountNote", "crm.updateAccountStatus"}, crm.Handle},
		{[]string{"ticket.createIssue", "ticket.getIssue", "ticket.searchIssues", "ticket.addComment", "ticket.transitionIssue"}, ticketing.Handle},
		{[]string{"messaging.listChannels", "messaging.readChannel", "messaging.postMessage"}, messaging.Handle},
	}
	for _, module := range modules {
		for _, key := range module.keys {
			if err := r.Register(key, module.handler); err != nil {
				panic(err)
			}
		}
	}
	return r
}
