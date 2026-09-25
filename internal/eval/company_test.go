package eval

import (
	"context"
	"reflect"
	"testing"

	"twinwright/internal/store"
)

func companyFixture(t *testing.T, scenario string) (*store.Store, string) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/eval.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := s.SeedScenario(context.Background(), 42, "digest", scenario)
	if err != nil {
		t.Fatal(err)
	}
	return s, w.ID
}
func execCompany(t *testing.T, s *store.Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func finishCompany(t *testing.T, s *store.Store, world, scenario string) {
	t.Helper()
	if scenario != "company-no-duplicate" {
		execCompany(t, s, "INSERT INTO refunds(world_id,id,charge_id,amount_cents,reason,created_at) SELECT world_id,'RF-eval',id,amount_cents,'Duplicate charge','2026-02-13T00:00:00Z' FROM charges WHERE world_id=? AND id='CH-1002'", world)
		execCompany(t, s, "UPDATE charges SET refunded_cents=amount_cents WHERE world_id=? AND id='CH-1002'", world)
	}
	execCompany(t, s, "INSERT INTO crm_notes VALUES(?,?,?,?,?)", world, "NOTE-eval", "A-104", "Billing investigation completed.", "2026-02-13T00:00:00Z")
	status := "resolved"
	if scenario == "company-incident" {
		status = "needs_followup"
	}
	execCompany(t, s, "UPDATE crm_accounts SET status=? WHERE world_id=? AND id='A-104'", status, world)
	if scenario == "company-incident" {
		execCompany(t, s, "INSERT INTO ticket_issues VALUES(?,?,?,?,?,?,?)", world, "ISSUE-eval", "PROJ-ENG", "A-104", "Investigate duplicate billing", "open", "high")
		execCompany(t, s, "INSERT INTO message_messages VALUES(?,?,?,?,?)", world, "MSG-eval", "CH-SUPPORT", "Investigate billing incident for C-104.", "2026-02-13T00:00:00Z")
	}
}
func companyReport(t *testing.T, s *store.Store, world, scenario string) Report {
	t.Helper()
	report, err := Evaluate(context.Background(), s, world, scenario)
	if err != nil {
		t.Fatal(err)
	}
	return report
}
func TestCompanyEvaluationRequiresActualCompletedState(t *testing.T) {
	for _, scenario := range []string{"company-incident", "company-routine", "company-no-duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			s, world := companyFixture(t, scenario)
			if report := companyReport(t, s, world, scenario); report.Passed {
				t.Fatalf("untouched world passed: %+v", report)
			}
			finishCompany(t, s, world, scenario)
			report := companyReport(t, s, world, scenario)
			if !report.Passed {
				t.Fatalf("valid state failed: %+v", report)
			}
			if again := companyReport(t, s, world, scenario); !reflect.DeepEqual(report, again) {
				t.Fatalf("nondeterministic reports: %+v %+v", report, again)
			}
		})
	}
}
func TestCompanyEvaluationRejectsIncorrectState(t *testing.T) {
	cases := []struct{ name, scenario, query string }{
		{"seeded note is not resolution", "company-incident", "DELETE FROM crm_notes WHERE world_id=? AND id='NOTE-eval'"},
		{"blank note", "company-routine", "UPDATE crm_notes SET body='   ' WHERE world_id=? AND id='NOTE-eval'"},
		{"wrong account note", "company-routine", "UPDATE crm_notes SET account_id='A-205' WHERE world_id=? AND id='NOTE-eval'"},
		{"wrong resolution status", "company-routine", "UPDATE crm_accounts SET status='active' WHERE world_id=? AND id='A-104'"},
		{"incident requires followup", "company-incident", "UPDATE crm_accounts SET status='resolved' WHERE world_id=? AND id='A-104'"},
		{"wrong customer link", "company-routine", "UPDATE crm_accounts SET customer_id='C-205' WHERE world_id=? AND id='A-104'"},
		{"wrong refund target", "company-routine", "UPDATE refunds SET charge_id='CH-1001' WHERE world_id=?"},
		{"partial refund", "company-routine", "UPDATE refunds SET amount_cents=1 WHERE world_id=?"},
		{"no duplicate forbids refund", "company-no-duplicate", "INSERT INTO refunds VALUES(?,'RF-wrong','CH-1001',1,'wrong','2026-02-13T00:00:00Z')"},
		{"no duplicate preserves charged amount", "company-no-duplicate", "UPDATE charges SET refunded_cents=1 WHERE world_id=? AND id='CH-1001'"},
		{"missing ticket", "company-incident", "DELETE FROM ticket_issues WHERE world_id=?"},
		{"wrong ticket account", "company-incident", "UPDATE ticket_issues SET account_id='A-205' WHERE world_id=?"},
		{"wrong ticket project", "company-incident", "UPDATE ticket_issues SET project_id='OTHER' WHERE world_id=?"},
		{"routine forbids ticket", "company-routine", "INSERT INTO ticket_issues VALUES(?,'ISSUE-wrong','PROJ-ENG','A-104','unneeded','open','high')"},
		{"no duplicate forbids ticket", "company-no-duplicate", "INSERT INTO ticket_issues VALUES(?,'ISSUE-wrong','PROJ-ENG','A-104','unneeded','open','high')"},
		{"missing message", "company-incident", "DELETE FROM message_messages WHERE world_id=?"},
		{"wrong message channel", "company-incident", "UPDATE message_messages SET channel_id='OTHER' WHERE world_id=?"},
		{"wrong customer message", "company-incident", "UPDATE message_messages SET body='Billing incident for C-205.' WHERE world_id=?"},
		{"routine forbids message", "company-routine", "INSERT INTO message_messages VALUES(?,'MSG-wrong','CH-SUPPORT','C-104','2026-02-13T00:00:00Z')"},
		{"no duplicate forbids message", "company-no-duplicate", "INSERT INTO message_messages VALUES(?,'MSG-wrong','CH-SUPPORT','C-104','2026-02-13T00:00:00Z')"},
		{"unrelated status", "company-routine", "UPDATE crm_accounts SET status='resolved' WHERE world_id=? AND id='A-205'"},
		{"unrelated note", "company-routine", "INSERT INTO crm_notes VALUES(?,'NOTE-wrong','A-205','unrelated','2026-02-13T00:00:00Z')"},
		{"unrelated refund total", "company-routine", "UPDATE charges SET refunded_cents=1 WHERE world_id=? AND id='CH-2001'"},
		{"unrelated customer", "company-routine", "UPDATE customers SET name='Changed' WHERE world_id=? AND id='C-205'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, world := companyFixture(t, tc.scenario)
			finishCompany(t, s, world, tc.scenario)
			execCompany(t, s, tc.query, world)
			if report := companyReport(t, s, world, tc.scenario); report.Passed {
				t.Fatalf("incorrect state passed: %+v", report)
			}
		})
	}
}
func TestCompanyEvaluationIsWorldScoped(t *testing.T) {
	s, world := companyFixture(t, "company-incident")
	other, err := s.SeedScenario(context.Background(), 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	finishCompany(t, s, other.ID, "company-incident")
	if report := companyReport(t, s, world, "company-incident"); report.Passed {
		t.Fatal("another world's work satisfied target")
	}
	finishCompany(t, s, world, "company-incident")
	execCompany(t, s, "UPDATE crm_accounts SET status='resolved' WHERE world_id=? AND id='A-205'", other.ID)
	if report := companyReport(t, s, world, "company-incident"); !report.Passed {
		t.Fatalf("other world affected result: %+v", report)
	}
}
func TestEvaluatePreservesDuplicateChargeChecks(t *testing.T) {
	s, world := companyFixture(t, "duplicate-charge")
	want, err := DuplicateCharge(context.Background(), s, world)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Evaluate(context.Background(), s, world, "duplicate-charge")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy changed: %+v versus %+v", got, want)
	}
	if _, err = Evaluate(context.Background(), s, world, "unknown"); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}
