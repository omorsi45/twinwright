package ticketing

import (
	"context"
	"strings"
	"testing"

	"twinwright/internal/store"
)

func ticketWorld(t *testing.T) (*store.Store, string) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.SeedScenario(context.Background(), 42, "digest", "company-incident")
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	return s, w.ID
}

func ticketCall(t *testing.T, s *store.Store, worldID, callID, behavior string, args map[string]any) (int, any, any) {
	t.Helper()
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	status, body, mutation, err := Handle(context.Background(), tx, worldID, "run-1", callID, behavior, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return status, body, mutation
}

func TestTicketIssueWorkflow(t *testing.T) {
	s, worldID := ticketWorld(t)
	defer s.Close()
	status, body, mutation := ticketCall(t, s, worldID, "create-1", "ticket.createIssue", map[string]any{
		"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Retry worker charged twice", "priority": "high",
	})
	if status != 201 || mutation == nil {
		t.Fatalf("create status=%d mutation=%v", status, mutation)
	}
	issue := body.(map[string]any)
	issueID, ok := issue["id"].(string)
	if !ok || !strings.HasPrefix(issueID, "ISSUE-") || issue["status"] != "open" || issue["account_id"] != "A-104" {
		t.Fatalf("created issue=%v", issue)
	}

	status, body, mutation = ticketCall(t, s, worldID, "get-1", "ticket.getIssue", map[string]any{"id": issueID})
	if status != 200 || mutation != nil {
		t.Fatalf("get status=%d mutation=%v", status, mutation)
	}
	got := body.(map[string]any)
	if got["id"] != issueID || len(got["comments"].([]map[string]any)) != 0 {
		t.Fatalf("get issue=%v", got)
	}

	status, body, mutation = ticketCall(t, s, worldID, "search-1", "ticket.searchIssues", map[string]any{"query": "retry"})
	if status != 200 || mutation != nil {
		t.Fatalf("search status=%d mutation=%v", status, mutation)
	}
	found := body.([]map[string]any)
	if len(found) != 1 || found[0]["id"] != issueID {
		t.Fatalf("search result=%v", found)
	}

	status, body, mutation = ticketCall(t, s, worldID, "comment-1", "ticket.addComment", map[string]any{"issue_id": issueID, "body": "Confirmed the worker retry path."})
	if status != 201 || mutation == nil {
		t.Fatalf("comment status=%d mutation=%v", status, mutation)
	}
	comment := body.(map[string]any)
	if !strings.HasPrefix(comment["id"].(string), "COMMENT-") || comment["issue_id"] != issueID || comment["created_at"] == "" {
		t.Fatalf("comment=%v", comment)
	}
	status, body, _ = ticketCall(t, s, worldID, "get-2", "ticket.getIssue", map[string]any{"id": issueID})
	if status != 200 {
		t.Fatalf("get after comment status=%d", status)
	}
	comments := body.(map[string]any)["comments"].([]map[string]any)
	if len(comments) != 1 || comments[0]["id"] != comment["id"] {
		t.Fatalf("comments=%v", comments)
	}

	for _, step := range []struct{ callID, target string }{{"transition-1", "in_progress"}, {"transition-2", "resolved"}} {
		status, body, mutation = ticketCall(t, s, worldID, step.callID, "ticket.transitionIssue", map[string]any{"issue_id": issueID, "status": step.target})
		if status != 200 || mutation == nil || body.(map[string]any)["status"] != step.target {
			t.Fatalf("transition to %s: status=%d body=%v mutation=%v", step.target, status, body, mutation)
		}
	}
	status, body, mutation = ticketCall(t, s, worldID, "transition-3", "ticket.transitionIssue", map[string]any{"issue_id": issueID, "status": "open"})
	if status != 409 || mutation != nil {
		t.Fatalf("invalid transition status=%d body=%v mutation=%v", status, body, mutation)
	}
	status, body, _ = ticketCall(t, s, worldID, "get-3", "ticket.getIssue", map[string]any{"id": issueID})
	if status != 200 || body.(map[string]any)["status"] != "resolved" {
		t.Fatalf("final issue=%v", body)
	}
}

func TestTicketValidationAndWorldIsolation(t *testing.T) {
	s, worldID := ticketWorld(t)
	defer s.Close()
	other, err := s.SeedScenario(context.Background(), 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	create := map[string]any{"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Duplicate billing", "priority": "high"}
	for _, tc := range []struct {
		name, behavior string
		args           map[string]any
		want           int
	}{
		{"unknown project", "ticket.createIssue", map[string]any{"project_id": "missing", "account_id": "A-104", "title": "Issue", "priority": "high"}, 404},
		{"unknown account", "ticket.createIssue", map[string]any{"project_id": "PROJ-ENG", "account_id": "missing", "title": "Issue", "priority": "high"}, 404},
		{"invalid priority", "ticket.createIssue", map[string]any{"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Issue", "priority": "urgent"}, 400},
		{"blank title", "ticket.createIssue", map[string]any{"project_id": "PROJ-ENG", "account_id": "A-104", "title": "  ", "priority": "high"}, 400},
		{"missing issue", "ticket.getIssue", map[string]any{"id": "missing"}, 404},
		{"missing comment issue", "ticket.addComment", map[string]any{"issue_id": "missing", "body": "Check"}, 404},
		{"missing transition issue", "ticket.transitionIssue", map[string]any{"issue_id": "missing", "status": "in_progress"}, 404},
		{"invalid status", "ticket.transitionIssue", map[string]any{"issue_id": "missing", "status": "blocked"}, 400},
		{"blank search", "ticket.searchIssues", map[string]any{"query": " "}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, mutation := ticketCall(t, s, worldID, tc.name, tc.behavior, tc.args)
			if status != tc.want || mutation != nil {
				t.Fatalf("status=%d mutation=%v, want %d and nil", status, mutation, tc.want)
			}
		})
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM ticket_issues WHERE world_id=?", worldID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid calls created %d issues", count)
	}

	_, body, _ := ticketCall(t, s, worldID, "create-valid", "ticket.createIssue", create)
	issueID := body.(map[string]any)["id"].(string)
	status, _, _ := ticketCall(t, s, other.ID, "other-get", "ticket.getIssue", map[string]any{"id": issueID})
	if status != 404 {
		t.Fatalf("other world can read issue: %d", status)
	}
	status, body, _ = ticketCall(t, s, other.ID, "other-search", "ticket.searchIssues", map[string]any{"query": "billing"})
	if status != 200 || len(body.([]map[string]any)) != 0 {
		t.Fatalf("other world search=%d %v", status, body)
	}
}

func TestTicketMutationRollsBackWithCallerTransaction(t *testing.T) {
	s, worldID := ticketWorld(t)
	defer s.Close()
	ctx := context.Background()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, body, mutation, err := Handle(ctx, tx, worldID, "run-1", "rolled-back", "ticket.createIssue", map[string]any{
		"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Temporary", "priority": "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != 201 || mutation == nil || body == nil {
		t.Fatalf("status=%d body=%v mutation=%v", status, body, mutation)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM ticket_issues WHERE world_id=?", worldID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rolled back issue left %d rows", count)
	}
}

func TestTicketIDsAndCommentTimeReproduceAcrossWorlds(t *testing.T) {
	s, firstWorld := ticketWorld(t)
	defer s.Close()
	second, err := s.SeedScenario(context.Background(), 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	var firstIssueID, firstCommentID, firstAt string
	for i, worldID := range []string{firstWorld, second.ID} {
		_, body, _ := ticketCall(t, s, worldID, "create-stable", "ticket.createIssue", map[string]any{
			"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Same issue", "priority": "medium",
		})
		issueID := body.(map[string]any)["id"].(string)
		_, body, _ = ticketCall(t, s, worldID, "comment-stable", "ticket.addComment", map[string]any{"issue_id": issueID, "body": "Same comment"})
		comment := body.(map[string]any)
		if i == 0 {
			firstIssueID, firstCommentID, firstAt = issueID, comment["id"].(string), comment["created_at"].(string)
		} else if issueID != firstIssueID || comment["id"] != firstCommentID || comment["created_at"] != firstAt {
			t.Fatalf("same seed and call IDs changed issue/comment fixture: issue=%q comment=%v", issueID, comment)
		}
	}
}
