package assertion

// Column kinds of the world-scoped tables assertions may read. Every SQL
// identifier in this package comes from this allowlist.
const (
	text    = "text"
	integer = "integer"
)

var tables = map[string]map[string]string{
	"customers":          {"id": text, "name": text},
	"invoices":           {"id": text, "customer_id": text, "amount_cents": integer, "subscription_id": text},
	"charges":            {"id": text, "invoice_id": text, "amount_cents": integer, "refunded_cents": integer, "created_at": text},
	"refunds":            {"id": text, "charge_id": text, "amount_cents": integer, "reason": text, "created_at": text},
	"subscriptions":      {"id": text, "customer_id": text, "status": text, "plan": text},
	"crm_accounts":       {"id": text, "customer_id": text, "status": text, "representative_id": text},
	"crm_contacts":       {"id": text, "account_id": text, "name": text, "email": text},
	"crm_notes":          {"id": text, "account_id": text, "body": text, "created_at": text},
	"ticket_projects":    {"id": text, "key": text, "name": text},
	"ticket_issues":      {"id": text, "project_id": text, "account_id": text, "title": text, "status": text, "priority": text},
	"ticket_comments":    {"id": text, "issue_id": text, "body": text, "created_at": text},
	"message_workspaces": {"id": text, "name": text},
	"message_channels":   {"id": text, "workspace_id": text, "name": text},
	"message_members":    {"channel_id": text, "principal_id": text},
	"message_messages":   {"id": text, "channel_id": text, "body": text, "created_at": text},
}

var eventTypes = map[string]bool{
	"execution.started": true, "execution.forked": true, "execution.paused": true, "execution.completed": true,
	"model.request": true, "model.response": true, "tool.request": true, "tool.response": true,
	"state.mutation": true, "error": true, "retry": true, "chaos.injected": true, "chaos.actor_mutation": true,
	"authorization.allowed": true, "authorization.denied": true,
}
