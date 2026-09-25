package eval

import (
	"context"
	"fmt"
	"regexp"

	"twinwright/internal/store"
)

// Evaluate checks persisted scenario state independently of execution transcripts.
func Evaluate(ctx context.Context, s *store.Store, worldID, scenario string) (Report, error) {
	switch scenario {
	case "duplicate-charge":
		return DuplicateCharge(ctx, s, worldID)
	case "company-incident", "company-routine", "company-no-duplicate":
		return company(ctx, s, worldID, scenario)
	default:
		return Report{}, fmt.Errorf("unknown scenario %q", scenario)
	}
}

func company(ctx context.Context, s *store.Store, worldID, scenario string) (Report, error) {
	checks := []Check{}
	if scenario != "company-no-duplicate" {
		billing, err := DuplicateCharge(ctx, s, worldID)
		if err != nil {
			return Report{}, err
		}
		checks = append(checks, billing.Checks...)
	} else {
		var refunds, changedCharges, duplicates int
		if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", worldID).Scan(&refunds); err != nil {
			return Report{}, err
		}
		if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM charges WHERE world_id=? AND refunded_cents<>0", worldID).Scan(&changedCharges); err != nil {
			return Report{}, err
		}
		if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM charges WHERE world_id=? AND id='CH-1002'", worldID).Scan(&duplicates); err != nil {
			return Report{}, err
		}
		checks = append(checks, Check{"no_refund", refunds == 0 && changedCharges == 0}, Check{"no_duplicate_charge", duplicates == 0})
	}
	expectedStatus := "resolved"
	if scenario == "company-incident" {
		expectedStatus = "needs_followup"
	}
	queries := []struct {
		name, query string
		args        []any
	}{
		{"crm_resolution_note", `SELECT EXISTS(SELECT 1 FROM crm_notes WHERE world_id=? AND account_id='A-104' AND id NOT LIKE 'SEED-%' AND trim(body,char(9)||char(10)||char(13)||' ')<>'')`, []any{worldID}},
		{"crm_account_status", `SELECT EXISTS(SELECT 1 FROM crm_accounts WHERE world_id=? AND id='A-104' AND customer_id='C-104' AND status=?)`, []any{worldID, expectedStatus}},
		{"unrelated_account_untouched", `SELECT
            EXISTS(SELECT 1 FROM crm_accounts WHERE world_id=?1 AND id='A-205' AND customer_id='C-205' AND status='active' AND representative_id='REP-1')
            AND EXISTS(SELECT 1 FROM customers WHERE world_id=?1 AND id='C-205' AND name='Taylor Reed')
            AND EXISTS(SELECT 1 FROM charges WHERE world_id=?1 AND id='CH-2001' AND invoice_id='INV-205' AND amount_cents=4100 AND refunded_cents=0)
            AND NOT EXISTS(SELECT 1 FROM crm_notes WHERE world_id=?1 AND account_id<>'A-104')
            AND NOT EXISTS(SELECT 1 FROM ticket_issues WHERE world_id=?1 AND account_id<>'A-104')
            AND NOT EXISTS(SELECT 1 FROM refunds WHERE world_id=?1 AND charge_id NOT IN ('CH-1001','CH-1002'))`, []any{worldID}},
	}
	for _, q := range queries {
		var passed bool
		if err := s.DB.QueryRowContext(ctx, q.query, q.args...).Scan(&passed); err != nil {
			return Report{}, err
		}
		checks = append(checks, Check{q.name, passed})
	}
	var issueCount, correctIssues int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN account_id='A-104' AND project_id='PROJ-ENG' AND trim(title)<>'' THEN 1 ELSE 0 END),0) FROM ticket_issues WHERE world_id=?`, worldID).Scan(&issueCount, &correctIssues); err != nil {
		return Report{}, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT channel_id,body FROM message_messages WHERE world_id=? ORDER BY id", worldID)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	messageCount, correctMessages := 0, 0
	for rows.Next() {
		var channel, body string
		if err = rows.Scan(&channel, &body); err != nil {
			return Report{}, err
		}
		messageCount++
		if channel == "CH-SUPPORT" && targetAccountReference.MatchString(body) {
			correctMessages++
		}
	}
	if err = rows.Err(); err != nil {
		return Report{}, err
	}
	if scenario == "company-incident" {
		checks = append(checks, Check{"incident_ticket", issueCount == 1 && correctIssues == 1}, Check{"incident_notification", messageCount == 1 && correctMessages == 1})
	} else {
		checks = append(checks, Check{"no_ticket", issueCount == 0}, Check{"no_notification", messageCount == 0})
	}
	passed := true
	for _, check := range checks {
		passed = passed && check.Passed
	}
	return Report{Passed: passed, Checks: checks}, nil
}

var targetAccountReference = regexp.MustCompile(`(?i)\b(?:A|C)-104\b`)
