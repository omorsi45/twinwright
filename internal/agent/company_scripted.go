package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"twinwright/internal/compiler"
)

// CompanyScriptedProvider exercises the company example without a model call.
type CompanyScriptedProvider struct{ Scenario string }

func (p CompanyScriptedProvider) Next(_ context.Context, _ string, history []Message, ops []compiler.Operation) (Message, error) {
	behaviorByFixtureID := map[string]string{
		"getCustomer": "billing.getCustomer", "getSubscription": "billing.getSubscription",
		"listInvoices": "billing.listInvoices", "listCharges": "billing.listCharges",
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
	for _, message := range history {
		if message.Role != "tool" {
			continue
		}
		tools = append(tools, message)
		if message.Status >= 200 && message.Status < 300 {
			fixtureID := fixtureIDByOperation[message.OperationID]
			if fixtureID == "" {
				fixtureID = message.OperationID
			}
			results[fixtureID] = message
		}
	}
	directCall := func(operation string, args map[string]any) (Message, error) {
		return Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: fmt.Sprintf("company-%d", len(tools)+1), OperationID: operation, Arguments: args,
		}}}, nil
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
		if last.Status < 200 || last.Status >= 300 {
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
	if duplicate {
		if _, ok := results["createRefund"]; !ok {
			return call("createRefund", map[string]any{"charge_id": charges[1].ID, "amount_cents": charges[1].AmountCents, "reason": "duplicate charge"})
		}
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
