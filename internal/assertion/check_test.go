package assertion

import (
	"context"
	"strings"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

type fixture struct {
	store    *store.Store
	run      store.Run
	manifest compiler.Manifest
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	manifest := manifestFrom(t, "company")
	s, err := store.Open(t.TempDir() + "/assert.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	world, err := s.SeedScenario(ctx, 42, manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{s, run, manifest}
}

func (f fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.DB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) check(t *testing.T, body string) Result {
	t.Helper()
	set, err := Parse([]byte("version: 1\nassertions:\n  - "+strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n    ")+"\n"), f.manifest, testCustoms)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Check(context.Background(), f.store, f.run.ID, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || report.Passed != report.Results[0].Passed || report.Digest != set.Digest() {
		t.Fatalf("report=%+v", report)
	}
	return report.Results[0]
}

func expect(t *testing.T, got Result, passed bool, detail string) {
	t.Helper()
	if got.Passed != passed || !strings.Contains(got.Detail, detail) {
		t.Fatalf("result=%+v, want passed=%v detail containing %q", got, passed, detail)
	}
}

func TestRowCountAssertions(t *testing.T) {
	f := newFixture(t)
	oneRefund := "id: a\ntype: row_count\ntable: refunds\nwhere: {charge_id: CH-1002}\nequals: 1"
	expect(t, f.check(t, oneRefund), false, "count 0, want equals 1")
	f.exec(t, "INSERT INTO refunds VALUES(?,?,?,?,?,?)", f.run.WorldID, "RF-1", "CH-1002", 500, "duplicate", "2026-01-01T00:00:00Z")
	expect(t, f.check(t, oneRefund), true, "")
	expect(t, f.check(t, "id: a\ntype: row_count\ntable: refunds\nwhere: {charge_id: CH-1002, amount_cents: 501}\nat_least: 1"), false, "count 0, want at_least 1")
	expect(t, f.check(t, "id: a\ntype: row_count\ntable: customers\nat_most: 1"), false, "count 2, want at_most 1")

	noLeak := "id: a\ntype: row_count\ntable: message_messages\nwhere: {body: {contains: C-205}}\nequals: 0"
	expect(t, f.check(t, noLeak), true, "")
	f.exec(t, "INSERT INTO message_messages VALUES(?,?,?,?,?)", f.run.WorldID, "MSG-1", "CH-SUPPORT", "details for C-205", "2026-01-01T00:00:00Z")
	expect(t, f.check(t, noLeak), false, "count 1")

	other, err := f.store.SeedScenario(context.Background(), 42, f.manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, "INSERT INTO refunds VALUES(?,?,?,?,?,?)", other.ID, "RF-2", "CH-1002", 500, "duplicate", "2026-01-01T00:00:00Z")
	expect(t, f.check(t, oneRefund), true, "")
}

func TestFieldEqualsAssertions(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents\nvalue: 0"), true, "")
	expect(t, f.check(t, "id: a\ntype: field_equals\nentity: crm_accounts.A-104\nfield: status\nvalue: resolved"), false, `got "active", want "resolved"`)
	fullRefund := "id: a\ntype: field_equals\nentity: charges.CH-1002\nfield: refunded_cents\nvalue_from: charges.CH-1002.amount_cents"
	expect(t, f.check(t, fullRefund), false, "want")
	f.exec(t, "UPDATE charges SET refunded_cents=amount_cents WHERE world_id=? AND id='CH-1002'", f.run.WorldID)
	expect(t, f.check(t, fullRefund), true, "")
	expect(t, f.check(t, "id: a\ntype: field_equals\nentity: charges.CH-9999\nfield: refunded_cents\nvalue: 0"), false, "charges.CH-9999 not found")
	expect(t, f.check(t, "id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents\nvalue_from: charges.CH-9999.amount_cents"), false, "charges.CH-9999 not found")
}

func TestRelationshipAssertions(t *testing.T) {
	f := newFixture(t)
	link := "id: a\ntype: relationship\ntable: refunds\nfield: charge_id\nreferences: charges"
	f.exec(t, "INSERT INTO refunds VALUES(?,?,?,?,?,?)", f.run.WorldID, "RF-1", "CH-1002", 500, "duplicate", "2026-01-01T00:00:00Z")
	expect(t, f.check(t, link), true, "")
	f.exec(t, "INSERT INTO refunds VALUES(?,?,?,?,?,?)", f.run.WorldID, "RF-2", "CH-9999", 500, "orphan", "2026-01-01T00:00:00Z")
	expect(t, f.check(t, link), false, "CH-9999")
	expect(t, f.check(t, "id: a\ntype: relationship\ntable: message_members\nfield: channel_id\nreferences: message_channels"), true, "")
}
