package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"twinwright/internal/compiler"
)

// CompanyScriptedProvider exercises the company example without a model call.
//
// Unsafe selects the careless branch, the way AmbiguousScriptedProvider does for
// the billing world, and the two fixtures differ by exactly one decision: what a
// lost write response means. The safe fixture reads the charge back before
// concluding anything; the unsafe one assumes its write landed and moves on to
// the paperwork. Under a fault that loses the response after committing, both
// end with a whole customer. Under one that loses it before committing, only the
// safe fixture does, and the unsafe run's CRM note then claims a refund the
// ledger does not contain. That is the failure the counterfactual step explains.
type CompanyScriptedProvider struct {
	Scenario string
	Unsafe   bool
}

func (p CompanyScriptedProvider) Next(_ context.Context, _ string, history []Message, ops []compiler.Operation) (Message, error) {
	behaviorByFixtureID := map[string]string{
		"getCustomer": "billing.getCustomer", "getSubscription": "billing.getSubscription",
		"listInvoices": "billing.listInvoices", "listCharges": "billing.listCharges",
		"getCharge":    "billing.getCharge",
		"createRefund": "billing.createRefund", "crmGetAccount": "crm.getAccount",
		"crmAddAccountNote": "crm.addAccountNote", "crmUpdateAccountStatus": "crm.updateAccountStatus",
		"ticketCreateIssue": "ticket.createIssue", "ticketAddComment": "ticket.addComment",
		"ticketTransitionIssue": "ticket.transitionIssue", "messageListChannels": "messaging.listChannels",
		"messageReadChannel": "messaging.readChannel", "messagePostMessage": "messaging.postMessage",
	}
	operationByBehavior := make(map[string]string, len(ops))
	for _, op := range ops {
		operationByBehavior[op.Behavior] = op.ID
	}
	fixtureIDByOperation := make(map[string]string, len(behaviorByFixtureID))
	for fixtureID, behavior := range behaviorByFixtureID {
		if operationID := operationByBehavior[behavior]; operationID != "" {
			fixtureIDByOperation[operationID] = fixtureID
		}
	}
	results := map[string]Message{}
	var tools []Message
	// A refund billing refused as exceeding what the charge still owes. The
	// provider rebuilds its decision from history on every turn, so this is
	// derived from the whole transcript rather than from the last message: the
	// rejection has to keep counting once the incident handling moves on.
	refundRefused := false
	// A refund whose response never arrived. Like refundRefused this is derived
	// from the whole transcript, because the decision it feeds has to hold for
	// every turn that follows it rather than only the one it happened on.
	refundLost := false
	for _, message := range history {
		if message.Role != "tool" {
			continue
		}
		tools = append(tools, message)
		fixtureID := fixtureIDByOperation[message.OperationID]
		if fixtureID == "" {
			fixtureID = message.OperationID
		}
		if message.Status >= 200 && message.Status < 300 {
			results[fixtureID] = message
		}
		if message.Status == 409 && fixtureID == "createRefund" {
			refundRefused = true
		}
		if message.Status == 0 && fixtureID == "createRefund" {
			refundLost = true
		}
	}
	directCall := func(operation string, args map[string]any) (Message, error) {
		return Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: fmt.Sprintf("company-%d", len(tools)+1), OperationID: operation, Arguments: args,
		}}}, nil
	}
	// What billing refused, and whether the charge has been read since. Both are
	// derived from transcript order rather than from the last message, because
	// the remainder decision below has to survive the turns that follow it.
	refusedAmount := int64(0)
	readsAfterRefusal := 0
	if refundRefused {
		refusalSeen := false
		for _, message := range history {
			if message.Role != "tool" {
				continue
			}
			fixtureID := fixtureIDByOperation[message.OperationID]
			if fixtureID == "" {
				fixtureID = message.OperationID
			}
			if !refusalSeen {
				if fixtureID == "createRefund" && message.Status == 409 {
					refusalSeen = true
					for _, earlier := range history {
						for _, previous := range earlier.ToolCalls {
							if previous.ID != message.CallID {
								continue
							}
							switch amount := previous.Arguments["amount_cents"].(type) {
							case int64:
								refusedAmount = amount
							case int:
								refusedAmount = int64(amount)
							case float64:
								refusedAmount = int64(amount)
							}
						}
					}
				}
				continue
			}
			if fixtureID == "getCharge" && message.Status >= 200 && message.Status < 300 {
				readsAfterRefusal++
			}
		}
	}
	call := func(operation string, args map[string]any) (Message, error) {
		if bound := operationByBehavior[behaviorByFixtureID[operation]]; bound != "" {
			operation = bound
		}
		return directCall(operation, args)
	}
	if len(tools) > 0 {
		last := tools[len(tools)-1]
		if last.Status == 503 {
			for i := len(history) - 1; i >= 0; i-- {
				for _, previous := range history[i].ToolCalls {
					if previous.ID == last.CallID {
						return directCall(previous.OperationID, previous.Arguments)
					}
				}
			}
			return Message{}, fmt.Errorf("missing failed call %s", last.CallID)
		}
		// Status 0 is a lost response, not a failed request: the write may
		// already have committed. Retrying blind would refund the customer
		// twice and giving up would leave the incident half-handled, so the
		// charge is read back before anything is decided. Only the refund is
		// treated this way, because it is the only call in this scenario whose
		// effect cannot be inferred from the rest of the investigation.
		if last.Status == 0 {
			lost := fixtureIDByOperation[last.OperationID]
			if lost == "" {
				lost = last.OperationID
			}
			if lost == "createRefund" && !p.Unsafe {
				chargeID := ""
				for i := len(history) - 1; i >= 0 && chargeID == ""; i-- {
					for _, previous := range history[i].ToolCalls {
						if previous.ID == last.CallID {
							if id, ok := previous.Arguments["charge_id"].(string); ok {
								chargeID = id
							}
						}
					}
				}
				if chargeID == "" {
					return Message{}, fmt.Errorf("lost refund response %s names no charge to reconcile", last.CallID)
				}
				return call("getCharge", map[string]any{"id": chargeID})
			}
		}
		// 409 on the refund is billing refusing to return MORE than the charge
		// still owes. It says the requested amount was too large, not that
		// nothing is owed: a charge another writer refunded in part refuses the
		// full amount exactly as loudly as a fully refunded one does, and the
		// gap between those two worlds is what the customer is still short. So
		// the refusal is neither a failure to stop on nor a confirmation. It is
		// handled below, by reading the charge once more and returning whatever
		// remains outstanding.
		switch {
		case last.Status == 409 && refundRefused:
			// Handled below, where the outstanding remainder is decided.
		case last.Status == 0 && refundLost && p.Unsafe:
			// Handled below. The unsafe branch does not stop on a lost response,
			// and it does not reconcile either; it assumes the write landed.
		case last.Status < 200 || last.Status >= 300:
			return Message{}, fmt.Errorf("%s returned HTTP %d", last.OperationID, last.Status)
		}
	}
	for _, next := range []struct {
		operation string
		args      map[string]any
	}{
		{"getCustomer", map[string]any{"id": "C-104"}},
		{"getSubscription", map[string]any{"id": "SUB-104"}},
		{"crmGetAccount", map[string]any{"id": "A-104"}},
		{"listInvoices", map[string]any{"id": "C-104"}},
		{"listCharges", map[string]any{"id": "INV-104"}},
	} {
		if _, ok := results[next.operation]; !ok {
			return call(next.operation, next.args)
		}
	}
	var charges []struct {
		ID          string `json:"id"`
		AmountCents int64  `json:"amount_cents"`
	}
	if err := json.Unmarshal([]byte(results["listCharges"].Content), &charges); err != nil {
		return Message{}, err
	}
	if len(charges) < 1 || len(charges) > 2 || charges[0].ID != "CH-1001" {
		return Message{}, fmt.Errorf("unexpected customer charge history")
	}
	duplicate := len(charges) == 2 && charges[1].AmountCents == charges[0].AmountCents
	if len(charges) == 2 && !duplicate {
		return Message{}, fmt.Errorf("charges require manual review")
	}
	var account struct {
		Notes []struct {
			Body string `json:"body"`
		} `json:"notes"`
	}
	if err := json.Unmarshal([]byte(results["crmGetAccount"].Content), &account); err != nil {
		return Message{}, err
	}
	incident := false
	for _, note := range account.Notes {
		incident = incident || strings.Contains(strings.ToLower(note.Body), "retry worker")
	}
	// Read the charge before moving any money. The invoice listing is a snapshot
	// from earlier in the investigation, and something else may have refunded this
	// charge since: another operator, a retry worker, a concurrent run. Refunding
	// without looking is how a charge that is already whole gets refunded twice.
	//
	// The same read serves reconciliation after a lost response, which is why the
	// check below looks at whichever getCharge result is most recent.
	if duplicate {
		if _, ok := results["getCharge"]; !ok {
			return call("getCharge", map[string]any{"id": charges[1].ID})
		}
	}
	// A lost refund response leaves the effect ambiguous, so a read-back that
	// shows the money already returned counts as the refund having happened.
	// The charge ID and amount are both checked: without them a partial or
	// unrelated refund could be mistaken for this one, which is the failure
	// this whole path exists to prevent.
	refundConfirmed := false
	if _, ok := results["createRefund"]; ok {
		refundConfirmed = true
	}
	// The unsafe fixture's whole divergence, in one statement: a lost response is
	// taken as proof the money moved. It is a tempting inference, because the
	// alternative is an extra read on a call that usually did succeed, and it is
	// wrong precisely when it matters.
	if p.Unsafe && refundLost {
		refundConfirmed = true
	}
	if reconciled, ok := results["getCharge"]; ok && duplicate {
		var charge struct {
			ID            string `json:"id"`
			RefundedCents int64  `json:"refunded_cents"`
		}
		if err := json.Unmarshal([]byte(reconciled.Content), &charge); err != nil {
			return Message{}, err
		}
		if charge.ID == charges[1].ID && charge.RefundedCents >= charges[1].AmountCents {
			refundConfirmed = true
		}
	}
	// Billing refused the refund as exceeding the outstanding balance. Read the
	// charge once more and return only what is still owed. That re-read is what
	// separates the two worlds a 409 cannot tell apart on its own: a charge
	// already whole leaves nothing to do, while one another writer refunded in
	// part leaves a remainder the customer is short. Reissuing the refused
	// amount is impossible here by construction, because a remainder billing
	// will accept is always smaller than the amount it just refused.
	if duplicate && refundRefused && !refundConfirmed {
		if readsAfterRefusal == 0 {
			return call("getCharge", map[string]any{"id": charges[1].ID})
		}
		var charge struct {
			ID            string `json:"id"`
			RefundedCents int64  `json:"refunded_cents"`
		}
		if err := json.Unmarshal([]byte(results["getCharge"].Content), &charge); err != nil {
			return Message{}, err
		}
		if charge.ID != charges[1].ID {
			return Message{}, fmt.Errorf("refused refund reconciled against charge %s", charge.ID)
		}
		remaining := charges[1].AmountCents - charge.RefundedCents
		switch {
		case remaining <= 0:
			refundConfirmed = true
		case refusedAmount > 0 && remaining >= refusedAmount:
			// The read claims at least as much is owed as billing just refused
			// to return. One of the two is wrong and the agent cannot tell
			// which, so it stops rather than reporting an incident resolved
			// that it has not resolved, or looping on the same refused amount.
			return Message{}, fmt.Errorf("charge %s reads %d outstanding after billing refused %d", charge.ID, remaining, refusedAmount)
		default:
			return call("createRefund", map[string]any{"charge_id": charges[1].ID, "amount_cents": remaining, "reason": "duplicate charge remainder"})
		}
	}
	if duplicate && !refundConfirmed {
		return call("createRefund", map[string]any{"charge_id": charges[1].ID, "amount_cents": charges[1].AmountCents, "reason": "duplicate charge"})
	}
	if _, ok := results["crmAddAccountNote"]; !ok {
		body := "Reviewed invoice and found one legitimate charge; no refund issued."
		if duplicate {
			body = "Refunded the duplicate charge after reviewing billing history."
		}
		if incident {
			body += " Billing retry worker incident requires engineering follow-up."
		}
		return call("crmAddAccountNote", map[string]any{"account_id": "A-104", "body": body})
	}
	if _, ok := results["crmUpdateAccountStatus"]; !ok {
		status := "resolved"
		if incident {
			status = "needs_followup"
		}
		return call("crmUpdateAccountStatus", map[string]any{"account_id": "A-104", "status": status})
	}
	if incident {
		if _, ok := results["ticketCreateIssue"]; !ok {
			return call("ticketCreateIssue", map[string]any{"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Investigate duplicate billing retry for A-104", "priority": "high"})
		}
		var issue struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(results["ticketCreateIssue"].Content), &issue); err != nil {
			return Message{}, err
		}
		if issue.ID == "" {
			return Message{}, fmt.Errorf("ticket creation returned no issue ID")
		}
		if _, ok := results["ticketAddComment"]; !ok {
			return call("ticketAddComment", map[string]any{"issue_id": issue.ID, "body": "CRM records a billing retry worker incident for A-104."})
		}
		if _, ok := results["ticketTransitionIssue"]; !ok {
			return call("ticketTransitionIssue", map[string]any{"issue_id": issue.ID, "status": "in_progress"})
		}
		if _, ok := results["messageListChannels"]; !ok {
			return call("messageListChannels", map[string]any{"id": "WS-1"})
		}
		var channels []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(results["messageListChannels"].Content), &channels); err != nil {
			return Message{}, err
		}
		found := false
		for _, channel := range channels {
			found = found || channel.ID == "CH-SUPPORT"
		}
		if !found {
			return Message{}, fmt.Errorf("support channel is unavailable")
		}
		if _, ok := results["messageReadChannel"]; !ok {
			return call("messageReadChannel", map[string]any{"id": "CH-SUPPORT"})
		}
		if _, ok := results["messagePostMessage"]; !ok {
			return call("messagePostMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "Billing retry incident for A-104: duplicate charge refunded and engineering issue opened."})
		}
	}
	return Message{Role: "assistant", Content: "Customer account reviewed and recorded."}, nil
}
