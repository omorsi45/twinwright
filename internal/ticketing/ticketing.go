package ticketing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Mutation struct {
	Kind      string `json:"kind"`
	IssueID   string `json:"issue_id,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
	Status    string `json:"status,omitempty"`
}

// Handle executes one explicitly bound ticketing behavior in the caller's transaction.
func Handle(ctx context.Context, tx *sql.Tx, worldID, runID, callID, behavior string, args map[string]any) (int, any, any, error) {
	switch behavior {
	case "ticket.createIssue":
		projectID, projectOK := stringArg(args, "project_id")
		accountID, accountOK := stringArg(args, "account_id")
		title, titleOK := stringArg(args, "title")
		priority, priorityOK := stringArg(args, "priority")
		if !projectOK || !accountOK || !titleOK || !priorityOK || !validPriority(priority) {
			return 400, map[string]string{"error": "invalid issue arguments"}, nil, nil
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ticket_projects WHERE world_id=? AND id=?", worldID, projectID).Scan(&count); err != nil {
			return 0, nil, nil, err
		}
		if count == 0 {
			return 404, map[string]string{"error": "project not found"}, nil, nil
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM crm_accounts WHERE world_id=? AND id=?", worldID, accountID).Scan(&count); err != nil {
			return 0, nil, nil, err
		}
		if count == 0 {
			return 404, map[string]string{"error": "account not found"}, nil, nil
		}
		issueID := stableID("ISSUE-", runID, callID)
		if _, err := tx.ExecContext(ctx, "INSERT INTO ticket_issues(world_id,id,project_id,account_id,title,status,priority) VALUES(?,?,?,?,?,?,?)", worldID, issueID, projectID, accountID, title, "open", priority); err != nil {
			return 0, nil, nil, err
		}
		issue := map[string]any{"id": issueID, "project_id": projectID, "account_id": accountID, "title": title, "status": "open", "priority": priority}
		return 201, issue, Mutation{Kind: "ticket.issue_created", IssueID: issueID}, nil

	case "ticket.getIssue":
		id, ok := stringArg(args, "id")
		if !ok {
			return 400, map[string]string{"error": "invalid id"}, nil, nil
		}
		issue, err := loadIssue(ctx, tx, worldID, id)
		if errors.Is(err, sql.ErrNoRows) {
			return 404, map[string]string{"error": "issue not found"}, nil, nil
		}
		if err != nil {
			return 0, nil, nil, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,body,created_at FROM ticket_comments WHERE world_id=? AND issue_id=? ORDER BY created_at,id", worldID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		defer rows.Close()
		comments := []map[string]any{}
		for rows.Next() {
			var commentID, body, at string
			if err = rows.Scan(&commentID, &body, &at); err != nil {
				return 0, nil, nil, err
			}
			comments = append(comments, map[string]any{"id": commentID, "issue_id": id, "body": body, "created_at": at})
		}
		if err = rows.Err(); err != nil {
			return 0, nil, nil, err
		}
		issue["comments"] = comments
		return 200, issue, nil, nil

	case "ticket.searchIssues":
		query, ok := stringArg(args, "query")
		if !ok {
			return 400, map[string]string{"error": "invalid query"}, nil, nil
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,project_id,account_id,title,status,priority FROM ticket_issues WHERE world_id=? AND instr(lower(id||' '||project_id||' '||account_id||' '||title||' '||status||' '||priority),lower(?))>0 ORDER BY id", worldID, strings.TrimSpace(query))
		if err != nil {
			return 0, nil, nil, err
		}
		defer rows.Close()
		issues := []map[string]any{}
		for rows.Next() {
			issue, err := scanIssue(rows)
			if err != nil {
				return 0, nil, nil, err
			}
			issues = append(issues, issue)
		}
		return 200, issues, nil, rows.Err()

	case "ticket.addComment":
		issueID, issueOK := stringArg(args, "issue_id")
		body, bodyOK := stringArg(args, "body")
		if !issueOK || !bodyOK {
			return 400, map[string]string{"error": "invalid comment arguments"}, nil, nil
		}
		if _, err := loadIssue(ctx, tx, worldID, issueID); errors.Is(err, sql.ErrNoRows) {
			return 404, map[string]string{"error": "issue not found"}, nil, nil
		} else if err != nil {
			return 0, nil, nil, err
		}
		commentID := stableID("COMMENT-", runID, callID)
		at, err := stableTime(ctx, tx, worldID, runID, callID)
		if err != nil {
			return 0, nil, nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO ticket_comments(world_id,id,issue_id,body,created_at) VALUES(?,?,?,?,?)", worldID, commentID, issueID, body, at); err != nil {
			return 0, nil, nil, err
		}
		comment := map[string]any{"id": commentID, "issue_id": issueID, "body": body, "created_at": at}
		return 201, comment, Mutation{Kind: "ticket.comment_added", IssueID: issueID, CommentID: commentID}, nil

	case "ticket.transitionIssue":
		issueID, issueOK := stringArg(args, "issue_id")
		target, targetOK := stringArg(args, "status")
		if !issueOK || !targetOK || (target != "open" && target != "in_progress" && target != "resolved") {
			return 400, map[string]string{"error": "invalid transition arguments"}, nil, nil
		}
		issue, err := loadIssue(ctx, tx, worldID, issueID)
		if errors.Is(err, sql.ErrNoRows) {
			return 404, map[string]string{"error": "issue not found"}, nil, nil
		}
		if err != nil {
			return 0, nil, nil, err
		}
		current := issue["status"].(string)
		if !(current == "open" && target == "in_progress" || current == "in_progress" && target == "resolved") {
			return 409, map[string]string{"error": "invalid issue transition"}, nil, nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE ticket_issues SET status=? WHERE world_id=? AND id=?", target, worldID, issueID); err != nil {
			return 0, nil, nil, err
		}
		issue["status"] = target
		return 200, issue, Mutation{Kind: "ticket.issue_transitioned", IssueID: issueID, Status: target}, nil
	default:
		return 0, nil, nil, fmt.Errorf("unknown ticket behavior %q", behavior)
	}
}

type scanner interface{ Scan(...any) error }

func scanIssue(row scanner) (map[string]any, error) {
	var id, projectID, accountID, title, status, priority string
	if err := row.Scan(&id, &projectID, &accountID, &title, &status, &priority); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "project_id": projectID, "account_id": accountID, "title": title, "status": status, "priority": priority}, nil
}

func loadIssue(ctx context.Context, tx *sql.Tx, worldID, id string) (map[string]any, error) {
	return scanIssue(tx.QueryRowContext(ctx, "SELECT id,project_id,account_id,title,status,priority FROM ticket_issues WHERE world_id=? AND id=?", worldID, id))
}

func stringArg(args map[string]any, key string) (string, bool) {
	value, ok := args[key].(string)
	return value, ok && strings.TrimSpace(value) != ""
}

func validPriority(priority string) bool {
	return priority == "low" || priority == "medium" || priority == "high"
}

func stableID(prefix, runID, callID string) string {
	sum := sha256.Sum256([]byte(runID + ":" + callID))
	return prefix + hex.EncodeToString(sum[:6])
}

func stableTime(ctx context.Context, tx *sql.Tx, worldID, runID, callID string) (string, error) {
	var base string
	if err := tx.QueryRowContext(ctx, "SELECT base_at FROM worlds WHERE id=?", worldID).Scan(&base); err != nil {
		return "", err
	}
	baseTime, err := time.Parse(time.RFC3339, base)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(runID + ":" + callID))
	offset := time.Duration(binary.BigEndian.Uint32(sum[6:10])%86400) * time.Second
	return baseTime.Add(24*time.Hour + offset).Format(time.RFC3339), nil
}
