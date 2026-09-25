package authz

import "strings"

// These mirror the argument checks built-in handlers make before reading
// state, beyond the dispatcher's schema validation. A secured run rejects such
// calls before a call number is consumed. Keep them in sync with the handlers.
var nonBlankArgs = map[string][]string{
	"crm.searchAccounts":      {"query"},
	"crm.getAccount":          {"id"},
	"crm.addAccountNote":      {"account_id", "body"},
	"crm.updateAccountStatus": {"account_id"},
	"ticket.createIssue":      {"project_id", "account_id", "title"},
	"ticket.getIssue":         {"id"},
	"ticket.searchIssues":     {"query"},
	"ticket.addComment":       {"issue_id", "body"},
	"ticket.transitionIssue":  {"issue_id"},
	"messaging.postMessage":   {"channel_id", "body"},
}

var allowedValues = map[string]map[string][]string{
	"crm.updateAccountStatus": {"status": {"active", "needs_followup", "resolved"}},
	"ticket.createIssue":      {"priority": {"low", "medium", "high"}},
	"ticket.transitionIssue":  {"status": {"open", "in_progress", "resolved"}},
}

func handlerValid(behavior string, args map[string]any) bool {
	for _, name := range nonBlankArgs[behavior] {
		if value, ok := args[name].(string); !ok || strings.TrimSpace(value) == "" {
			return false
		}
	}
	for name, values := range allowedValues[behavior] {
		if value, _ := args[name].(string); !contains(values, value) {
			return false
		}
	}
	if behavior == "billing.createRefund" {
		amount, ok := wholeNumber(args["amount_cents"])
		return ok && amount > 0
	}
	return true
}
