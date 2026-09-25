package replay

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

type Report struct {
	RunID          string `json:"run_id"`
	Verified       bool   `json:"verified"`
	ModelTurns     int    `json:"model_turns"`
	ToolCalls      int    `json:"tool_calls"`
	EventsCompared int    `json:"events_compared"`
	Divergence     string `json:"divergence,omitempty"`
}

type recordedProvider struct {
	messages []agent.Message
	next     int
}

func (p *recordedProvider) Next(_ context.Context, _ string, _ []agent.Message, _ []compiler.Operation) (agent.Message, error) {
	if p.next >= len(p.messages) {
		return agent.Message{}, fmt.Errorf("recorded model responses exhausted")
	}
	message := p.messages[p.next]
	p.next++
	return message, nil
}

func Verify(ctx context.Context, source *store.Store, runID string, manifest compiler.Manifest) (Report, error) {
	report := Report{RunID: runID}
	if err := compiler.ValidateManifest(manifest); err != nil {
		return report, fmt.Errorf("manifest: %w", err)
	}
	original, err := source.Run(ctx, runID)
	if err != nil {
		return report, err
	}
	if original.Status != "completed" {
		return report, fmt.Errorf("run %s is %s; replay requires a completed run", runID, original.Status)
	}
	switch original.Scenario {
	case "duplicate-charge", "ambiguous-commit", "company-incident", "company-routine", "company-no-duplicate", "prompt-injection-ticket":
	default:
		return report, fmt.Errorf("unsupported scenario %q", original.Scenario)
	}
	var seed int64
	var digest string
	if err = source.DB.QueryRowContext(ctx, "SELECT seed,digest FROM worlds WHERE id=?", original.WorldID).Scan(&seed, &digest); err != nil {
		return report, err
	}
	if digest != manifest.Digest {
		return report, fmt.Errorf("manifest mismatch for run %s", runID)
	}
	policyJSON, policyDigest, policyErr := source.ChaosPolicy(ctx, runID)
	if policyErr != nil && policyErr != sql.ErrNoRows {
		return report, policyErr
	}
	authJSON, authDigest, authErr := source.AuthPolicy(ctx, runID)
	if authErr != nil && authErr != sql.ErrNoRows {
		return report, authErr
	}
	if authErr == nil {
		policy, err := authz.ValidateStored(authJSON, authDigest, manifest)
		if err != nil {
			return diverged(report, err.Error()), nil
		}
		if policy.Principal.ID != original.PrincipalID {
			return diverged(report, "run principal differs from its authorization policy"), nil
		}
	} else {
		var authTables int
		if err := source.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='run_auth'").Scan(&authTables); err != nil {
			return report, err
		}
		allowed := original.PrincipalID == store.UnrestrictedPrincipal || authTables == 0 && original.PrincipalID == store.LegacyPrincipal
		if !allowed {
			return diverged(report, "run principal has no authorization policy"), nil
		}
	}
	lineage, lineageErr := source.Lineage(ctx, runID)
	if lineageErr != nil && lineageErr != sql.ErrNoRows {
		return report, lineageErr
	}
	forked := lineageErr == nil
	sourceEvents, err := source.Events(ctx, runID)
	if err != nil {
		return report, err
	}
	messages, err := recordedMessages(original, sourceEvents, forked)
	if err != nil {
		return report, err
	}
	report.ModelTurns = len(messages)

	var target *store.Store
	var worldID string
	if forked {
		target, err = fork.ReconstructForReplay(ctx, source, original, lineage, manifest)
		worldID = original.WorldID
	} else {
		target, err = store.Open(":memory:")
		if err == nil {
			var world store.World
			world, err = target.SeedScenario(ctx, seed, digest, original.Scenario)
			if err == nil {
				worldID = world.ID
				_, err = target.CreateReplayRun(ctx, original, world.ID)
				if err == nil && policyErr == nil {
					err = target.AttachChaos(ctx, original.ID, policyJSON, policyDigest)
				}
				if err == nil && authErr == nil {
					err = target.AttachAuth(ctx, original.ID, authJSON, authDigest)
				}
			}
		}
	}
	if err != nil {
		if target != nil {
			target.Close()
		}
		return report, err
	}
	defer target.Close()
	provider := &recordedProvider{messages: messages}
	runner := agent.Runner{
		Store: target, Dispatch: &dispatch.Dispatcher{Store: target, Manifest: manifest},
		Manifest: manifest, Provider: provider,
	}
	replayed, err := runner.Execute(ctx, runID, len(messages)+1)
	if ctx.Err() != nil {
		return report, ctx.Err()
	}
	if err != nil {
		return diverged(report, fmt.Sprintf("replayed execution: %v", err)), nil
	}
	if replayed.Status != "completed" || provider.next != len(messages) {
		return diverged(report, fmt.Sprintf("replayed run ended %s after %d of %d model turns", replayed.Status, provider.next, len(messages))), nil
	}
	targetEvents, err := target.Events(ctx, runID)
	if err != nil {
		return report, err
	}
	if forked && (len(sourceEvents) == 0 || len(targetEvents) == 0 ||
		sourceEvents[0].Type != "execution.forked" || sourceEvents[0].WorldAt != targetEvents[0].WorldAt ||
		!sameJSON(sourceEvents[0].Payload, targetEvents[0].Payload)) {
		return diverged(report, "fork origin event differs from lineage"), nil
	}
	if difference := compareEvents(sourceEvents, targetEvents); difference != "" {
		return diverged(report, difference), nil
	}
	report.EventsCompared = countSemantic(sourceEvents)
	sourceResults, err := toolResults(ctx, source.DB, runID)
	if err != nil {
		return report, err
	}
	targetResults, err := toolResults(ctx, target.DB, runID)
	if err != nil {
		return report, err
	}
	report.ToolCalls = len(sourceResults)
	if difference := compareResults(sourceResults, targetResults); difference != "" {
		return diverged(report, difference), nil
	}
	if difference, err := compareChaosState(ctx, source.DB, target.DB, runID); err != nil {
		return report, err
	} else if difference != "" {
		return diverged(report, difference), nil
	}
	if !sameJSON([]byte(original.Transcript), []byte(replayed.Transcript)) {
		return diverged(report, "final transcript differs"), nil
	}
	type tableSpec struct{ name, columns, orderBy string }
	tables := []tableSpec{
		{"customers", "id,name", "id"},
		{"invoices", "id,customer_id,amount_cents,subscription_id", "id"},
		{"charges", "id,invoice_id,amount_cents,refunded_cents,created_at", "id"},
		{"refunds", "id,charge_id,amount_cents,reason,created_at", "id"},
	}
	if original.Scenario != "duplicate-charge" && original.Scenario != "ambiguous-commit" {
		tables = append(tables, []tableSpec{
			{"subscriptions", "id,customer_id,status,plan", "id"},
			{"crm_accounts", "id,customer_id,status,representative_id", "id"},
			{"crm_contacts", "id,account_id,name,email", "id"},
			{"crm_notes", "id,account_id,body,created_at", "id"},
			{"ticket_projects", "id,key,name", "id"},
			{"ticket_issues", "id,project_id,account_id,title,status,priority", "id"},
			{"ticket_comments", "id,issue_id,body,created_at", "id"},
			{"message_workspaces", "id,name", "id"},
			{"message_channels", "id,workspace_id,name", "id"},
			{"message_members", "channel_id,principal_id", "channel_id,principal_id"},
			{"message_messages", "id,channel_id,body,created_at", "id"},
		}...)
	}
	for _, table := range tables {
		left, err := stateRows(ctx, source.DB, table.name, table.columns, table.orderBy, original.WorldID)
		if err != nil {
			return report, err
		}
		right, err := stateRows(ctx, target.DB, table.name, table.columns, table.orderBy, worldID)
		if err != nil {
			return report, err
		}
		if !reflect.DeepEqual(left, right) {
			return diverged(report, table.name+" state differs"), nil
		}
	}
	report.Verified = true
	return report, nil
}

func diverged(report Report, reason string) Report {
	report.Divergence = reason
	return report
}

func recordedMessages(run store.Run, events []store.Event, forked bool) ([]agent.Message, error) {
	startType := "execution.started"
	if forked {
		startType = "execution.forked"
	}
	if len(events) == 0 || events[0].Type != startType || events[len(events)-1].Type != "execution.completed" {
		return nil, fmt.Errorf("run ledger is missing start or completion")
	}
	var messages []agent.Message
	requests := 0
	for i, event := range events {
		if event.Seq != i+1 || event.ID != fmt.Sprintf("%s/%d", run.ID, i+1) || event.RunID != run.ID {
			return nil, fmt.Errorf("ledger sequence or ID differs at position %d", i+1)
		}
		if !json.Valid(event.Payload) {
			return nil, fmt.Errorf("invalid event payload at sequence %d", event.Seq)
		}
		switch event.Type {
		case "execution.forked":
			if !forked || i != 0 {
				return nil, fmt.Errorf("unexpected fork event at sequence %d", event.Seq)
			}
		case "observation.overridden":
			if !forked || i != 1 {
				return nil, fmt.Errorf("unexpected observation override at sequence %d", event.Seq)
			}
		case "execution.started":
			if forked || i != 0 {
				return nil, fmt.Errorf("unexpected start event at sequence %d", event.Seq)
			}
			var detail map[string]string
			if err := json.Unmarshal(event.Payload, &detail); err != nil {
				return nil, err
			}
			if detail["scenario"] != run.Scenario || detail["provider"] != run.Provider ||
				detail["model"] != run.Model || detail["world_id"] != run.WorldID {
				return nil, fmt.Errorf("execution start metadata differs from run")
			}
		case "execution.completed":
			if i != len(events)-1 {
				return nil, fmt.Errorf("completion event is not last")
			}
		case "execution.paused", "tool.request", "tool.response", "state.mutation", "retry", "chaos.injected", "chaos.actor_mutation", "authorization.allowed", "authorization.denied":
		case "model.request":
			requests++
		case "model.response":
			var message agent.Message
			decoder := json.NewDecoder(bytes.NewReader(event.Payload))
			decoder.UseNumber()
			if err := decoder.Decode(&message); err != nil {
				return nil, fmt.Errorf("model response at sequence %d: %w", event.Seq, err)
			}
			if message.Role != "assistant" {
				return nil, fmt.Errorf("model response at sequence %d is not a completed assistant turn", event.Seq)
			}
			messages = append(messages, message)
		case "error":
			var detail map[string]any
			if err := json.Unmarshal(event.Payload, &detail); err != nil {
				return nil, err
			}
			if detail["kind"] != "injected_503" {
				return nil, fmt.Errorf("run contains unsupported %v error at sequence %d", detail["kind"], event.Seq)
			}
		default:
			return nil, fmt.Errorf("unknown event type %q at sequence %d", event.Type, event.Seq)
		}
	}
	if requests != len(messages) || (!forked && len(messages) == 0) {
		return nil, fmt.Errorf("run has incomplete model history")
	}
	return messages, nil
}

func semantic(typ string) bool {
	switch typ {
	case "model.request", "model.response", "tool.request", "tool.response", "state.mutation", "error", "retry", "chaos.injected", "chaos.actor_mutation", "authorization.allowed", "authorization.denied", "observation.overridden":
		return true
	}
	return false
}

func countSemantic(events []store.Event) int {
	n := 0
	for _, e := range events {
		if semantic(e.Type) {
			n++
		}
	}
	return n
}

func compareEvents(left, right []store.Event) string {
	a, b := []store.Event{}, []store.Event{}
	for _, e := range left {
		if semantic(e.Type) {
			a = append(a, e)
		}
	}
	for _, e := range right {
		if semantic(e.Type) {
			b = append(b, e)
		}
	}
	if len(a) != len(b) {
		return fmt.Sprintf("semantic event count differs: recorded %d, replayed %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Type != b[i].Type || !sameJSON(a[i].Payload, b[i].Payload) {
			return fmt.Sprintf("event %d (%s) differs", a[i].Seq, a[i].Type)
		}
	}
	return ""
}

func sameJSON(a, b []byte) bool {
	if !json.Valid(a) || !json.Valid(b) {
		return false
	}
	decode := func(data []byte) any {
		var value any
		reader := json.NewDecoder(bytes.NewReader(data))
		reader.UseNumber()
		_ = reader.Decode(&value)
		return value
	}
	return reflect.DeepEqual(decode(a), decode(b))
}

type savedResult struct {
	callID, operation, arguments, body string
	status                             int
}

func toolResults(ctx context.Context, db *sql.DB, runID string) ([]savedResult, error) {
	rows, err := db.QueryContext(ctx, "SELECT call_id,operation_id,arguments,status,body FROM tool_results WHERE run_id=? ORDER BY call_id", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []savedResult
	for rows.Next() {
		var value savedResult
		if err = rows.Scan(&value.callID, &value.operation, &value.arguments, &value.status, &value.body); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func compareResults(left, right []savedResult) string {
	if len(left) != len(right) {
		return fmt.Sprintf("tool result count differs: recorded %d, replayed %d", len(left), len(right))
	}
	for i := range left {
		if left[i].callID != right[i].callID || left[i].operation != right[i].operation ||
			left[i].status != right[i].status ||
			!sameJSON([]byte(left[i].arguments), []byte(right[i].arguments)) ||
			!sameJSON([]byte(left[i].body), []byte(right[i].body)) {
			return fmt.Sprintf("tool result for call %s differs", left[i].callID)
		}
	}
	return ""
}

func compareChaosState(ctx context.Context, source, target *sql.DB, runID string) (string, error) {
	tables := []struct{ name, columns, order string }{
		{"run_chaos", "policy_json,digest", "run_id"},
		{"chaos_rule_state", "rule_id,matching_calls,injections", "rule_id"},
		{"chaos_snapshots", "rule_id,arguments_digest,status,body", "rule_id,arguments_digest"},
		{"chaos_hidden_outcomes", "call_id,rule_id,status,body", "call_id"},
		{"run_auth", "policy_json,digest", "run_id"},
		{"auth_state", "call_index", "run_id"},
	}
	for _, table := range tables {
		var left [][]string
		var exists int
		if err := source.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table.name).Scan(&exists); err != nil {
			return "", err
		}
		if exists != 0 {
			var err error
			left, err = stateRows(ctx, source, table.name, table.columns, table.order, runID)
			if err != nil {
				return "", err
			}
		}
		right, err := stateRows(ctx, target, table.name, table.columns, table.order, runID)
		if err != nil {
			return "", err
		}
		if !reflect.DeepEqual(left, right) {
			return table.name + " state differs", nil
		}
	}
	return "", nil
}

func stateRows(ctx context.Context, db *sql.DB, table, columns, orderBy, worldID string) ([][]string, error) {
	key := "world_id"
	switch table {
	case "run_chaos", "chaos_rule_state", "chaos_snapshots", "chaos_hidden_outcomes", "run_auth", "auth_state":
		key = "run_id"
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s=? ORDER BY %s", columns, table, key, orderBy)
	rows, err := db.QueryContext(ctx, query, worldID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var result [][]string
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return nil, err
		}
		record := make([]string, len(names))
		for i, value := range values {
			record[i] = fmt.Sprint(value)
		}
		result = append(result, record)
	}
	return result, rows.Err()
}
