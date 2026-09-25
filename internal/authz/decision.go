package authz

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"twinwright/internal/compiler"
)

// Stable reason codes recorded in authorization.denied events.
const (
	ReasonUnmapped      = "behavior_unmapped"
	ReasonNotGranted    = "permission_not_granted"
	ReasonRevoked       = "permission_revoked"
	ReasonCustomerScope = "customer_out_of_scope"
	ReasonChannelScope  = "channel_out_of_scope"
	ReasonProjectScope  = "project_out_of_scope"
	ReasonBroadSearch   = "broad_search_scoped"
	ReasonRefundLimit   = "refund_limit_exceeded"
)

// Decision is Enforced only for runs with a stored policy. Call is the
// run-local call number the decision consumed.
type Decision struct {
	Enforced   bool
	Allowed    bool
	Principal  string
	Permission string
	Reason     string
	Call       int
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const (
	chargeCustomer  = "SELECT i.customer_id FROM charges c JOIN invoices i ON i.world_id=c.world_id AND i.id=c.invoice_id WHERE c.world_id=? AND c.id=?"
	accountCustomer = "SELECT customer_id FROM crm_accounts WHERE world_id=? AND id=?"
	issueCustomer   = "SELECT a.customer_id FROM ticket_issues t JOIN crm_accounts a ON a.world_id=t.world_id AND a.id=t.account_id WHERE t.world_id=? AND t.id=?"
)

// customerLookups names the argument that identifies each behavior's resource
// and the query resolving that resource to its customer.
var customerLookups = map[string]struct{ arg, query string }{
	"billing.getCustomer":     {"id", "SELECT id FROM customers WHERE world_id=? AND id=?"},
	"billing.listInvoices":    {"id", "SELECT id FROM customers WHERE world_id=? AND id=?"},
	"billing.getSubscription": {"id", "SELECT customer_id FROM subscriptions WHERE world_id=? AND id=?"},
	"billing.listCharges":     {"id", "SELECT customer_id FROM invoices WHERE world_id=? AND id=?"},
	"billing.getCharge":       {"id", chargeCustomer},
	"billing.createRefund":    {"charge_id", chargeCustomer},
	"crm.getAccount":          {"id", accountCustomer},
	"crm.addAccountNote":      {"account_id", accountCustomer},
	"crm.updateAccountStatus": {"account_id", accountCustomer},
	"ticket.createIssue":      {"account_id", accountCustomer},
	"ticket.getIssue":         {"id", issueCustomer},
	"ticket.addComment":       {"issue_id", issueCustomer},
	"ticket.transitionIssue":  {"issue_id", issueCustomer},
}

var channelArgs = map[string]string{"messaging.readChannel": "id", "messaging.postMessage": "channel_id"}

// broad reports behaviors whose results span resources a scoped policy cannot narrow.
func (p Policy) broad(behavior string) bool {
	switch behavior {
	case "crm.searchAccounts", "ticket.searchIssues":
		return p.Resources.CustomerIDs != nil
	case "messaging.listChannels":
		return p.Resources.ChannelIDs != nil
	}
	return false
}

func load(ctx context.Context, q querier, runID string) (Policy, bool, error) {
	var encoded, digest string
	err := q.QueryRowContext(ctx, "SELECT policy_json,digest FROM run_auth WHERE run_id=?", runID).Scan(&encoded, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	hash := sha256.Sum256([]byte(encoded))
	if hex.EncodeToString(hash[:]) != digest {
		return Policy{}, false, fmt.Errorf("authorization policy digest mismatch for run %s", runID)
	}
	var policy Policy
	if err := json.Unmarshal([]byte(encoded), &policy); err != nil {
		return Policy{}, false, err
	}
	if policy.Version != 1 {
		return Policy{}, false, fmt.Errorf("unsupported stored authorization policy version %d", policy.Version)
	}
	return policy, true, nil
}

// grantReason returns "" when permission is active at call.
func (p Policy) grantReason(permission string, call int) string {
	active := contains(p.Permissions.Allow, permission)
	for _, role := range p.Principal.Roles {
		active = active || contains(p.Roles[role].Allow, permission)
	}
	for _, grant := range p.TemporaryGrants {
		active = active || grant.Permission == permission && grant.StartsAtCall <= call && call <= grant.EndsAtCall
	}
	if !active {
		return ReasonNotGranted
	}
	for _, revocation := range p.Revocations {
		if revocation.Permission == permission && call > revocation.AfterCall {
			return ReasonRevoked
		}
	}
	return ""
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Decide consumes one call number for a run with a policy. Call it inside the
// dispatcher transaction, after argument validation and saved-result lookup.
func Decide(ctx context.Context, tx *sql.Tx, runID, worldID string, op compiler.Operation, args map[string]any) (Decision, error) {
	policy, enforced, err := load(ctx, tx, runID)
	if err != nil || !enforced {
		return Decision{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_state(run_id,call_index) VALUES(?,1) ON CONFLICT(run_id) DO UPDATE SET call_index=call_index+1`, runID); err != nil {
		return Decision{}, err
	}
	decision := Decision{Enforced: true, Principal: policy.Principal.ID}
	if err := tx.QueryRowContext(ctx, "SELECT call_index FROM auth_state WHERE run_id=?", runID).Scan(&decision.Call); err != nil {
		return Decision{}, err
	}
	permission, mapped := Permission(op)
	if !mapped {
		decision.Reason = ReasonUnmapped
		return decision, nil
	}
	decision.Permission = permission
	if decision.Reason = policy.grantReason(permission, decision.Call); decision.Reason != "" {
		return decision, nil
	}
	if decision.Reason, err = policy.resourceReason(ctx, tx, worldID, op.Behavior, args); err != nil {
		return Decision{}, err
	}
	decision.Allowed = decision.Reason == ""
	return decision, nil
}

func (p Policy) resourceReason(ctx context.Context, q querier, worldID, behavior string, args map[string]any) (string, error) {
	if p.broad(behavior) {
		return ReasonBroadSearch, nil
	}
	if p.Resources.CustomerIDs != nil {
		customer, scoped, err := CustomerOf(ctx, q, worldID, behavior, args)
		if err != nil {
			return "", err
		}
		if scoped && !contains(*p.Resources.CustomerIDs, customer) {
			return ReasonCustomerScope, nil
		}
	}
	if arg, ok := channelArgs[behavior]; ok && p.Resources.ChannelIDs != nil {
		if channel, _ := args[arg].(string); !contains(*p.Resources.ChannelIDs, channel) {
			return ReasonChannelScope, nil
		}
	}
	if behavior == "ticket.createIssue" && p.Resources.ProjectIDs != nil {
		if project, _ := args["project_id"].(string); !contains(*p.Resources.ProjectIDs, project) {
			return ReasonProjectScope, nil
		}
	}
	if behavior == "billing.createRefund" && p.Constraints.RefundMaxCents != nil {
		amount, ok := wholeNumber(args["amount_cents"])
		if !ok || amount > int64(*p.Constraints.RefundMaxCents) {
			return ReasonRefundLimit, nil
		}
	}
	return "", nil
}

// CustomerOf resolves the customer a call's resource belongs to. scoped is
// false for behaviors without a customer resource; customer is "" when the
// resource does not exist in the world.
func CustomerOf(ctx context.Context, q querier, worldID, behavior string, args map[string]any) (customer string, scoped bool, err error) {
	lookup, ok := customerLookups[behavior]
	if !ok {
		return "", false, nil
	}
	err = q.QueryRowContext(ctx, lookup.query, worldID, args[lookup.arg]).Scan(&customer)
	if errors.Is(err, sql.ErrNoRows) {
		return "", true, nil
	}
	return customer, true, err
}

func wholeNumber(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n == float64(int64(n))
	case json.Number:
		parsed, err := n.Int64()
		return parsed, err == nil
	}
	return 0, false
}

// Exposed lists the operations a provider should see before the next call.
// It only filters by permission; the dispatcher still enforces every call.
func Exposed(ctx context.Context, q querier, runID string, ops []compiler.Operation) ([]compiler.Operation, error) {
	policy, enforced, err := load(ctx, q, runID)
	if err != nil || !enforced {
		return ops, err
	}
	var call int
	err = q.QueryRowContext(ctx, "SELECT call_index FROM auth_state WHERE run_id=?", runID).Scan(&call)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	out := []compiler.Operation{}
	for _, op := range ops {
		permission, mapped := Permission(op)
		if mapped && policy.grantReason(permission, call+1) == "" && !policy.broad(op.Behavior) {
			out = append(out, op)
		}
	}
	return out, nil
}
