package assertion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

func manifestFrom(t *testing.T, example string) compiler.Manifest {
	t.Helper()
	root := filepath.Join("..", "..", "examples", example)
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func alwaysPass(context.Context, *store.Store, store.Run) (bool, string, error) { return true, "", nil }

var testCustoms = Customs{"always": alwaysPass}

const everyType = `version: 1
assertions:
  - id: one_refund
    type: row_count
    table: refunds
    where: {charge_id: CH-1002}
    equals: 1
  - id: no_leak
    type: row_count
    table: message_messages
    where: {body: {contains: C-205}}
    at_most: 0
  - id: legit_untouched
    type: field_equals
    entity: charges.CH-1001
    field: refunded_cents
    value: 0
  - id: full_refund
    type: field_equals
    entity: charges.CH-1002
    field: refunded_cents
    value_from: charges.CH-1002.amount_cents
  - id: refunds_link
    type: relationship
    table: refunds
    field: charge_id
    references: charges
  - id: denied
    type: event_exists
    event: authorization.denied
    where: {operation_id: getCustomer, reason: customer_out_of_scope}
  - id: no_errors
    type: event_absent
    event: error
  - id: two_reads
    type: event_count
    event: tool.request
    where: {operation_id: getCustomer}
    at_least: 2
  - id: no_messages
    type: mutation_forbidden
    service: messaging
  - id: no_status_change
    type: mutation_forbidden
    operation: crmUpdateAccountStatus
  - id: read_first
    type: event_order
    first: {event: tool.response, where: {operation_id: ticketGetIssue}}
    then: {event: tool.request, where: {operation_id: ticketAddComment}}
  - id: custom
    type: custom
    name: always
`

func TestParseEveryAssertionType(t *testing.T) {
	manifest := manifestFrom(t, "company")
	set, err := Parse([]byte(everyType), manifest, testCustoms)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.assertions) != 12 || set.Digest() == "" {
		t.Fatalf("set=%+v", set)
	}
	again, err := Parse([]byte(everyType), manifest, testCustoms)
	if err != nil || again.Digest() != set.Digest() {
		t.Fatalf("digest not deterministic: %v", err)
	}
	changed, err := Parse([]byte(strings.Replace(everyType, "equals: 1", "equals: 2", 1)), manifest, testCustoms)
	if err != nil || changed.Digest() == set.Digest() {
		t.Fatalf("changed bound kept digest: %v", err)
	}
}

func TestDigestCoversManifest(t *testing.T) {
	file := []byte("version: 1\nassertions: [{id: a, type: event_absent, event: error}]\n")
	company, err := Parse(file, manifestFrom(t, "company"), testCustoms)
	if err != nil {
		t.Fatal(err)
	}
	billing, err := Parse(file, manifestFrom(t, "billing"), testCustoms)
	if err != nil {
		t.Fatal(err)
	}
	if company.Digest() == billing.Digest() {
		t.Fatal("digest ignores the manifest")
	}
}

func TestParseAcceptsObservationOverrideEvents(t *testing.T) {
	file := []byte("version: 1\nassertions: [{id: saw_real_response, type: event_absent, event: observation.overridden}]\n")
	if _, err := Parse(file, manifestFrom(t, "billing"), testCustoms); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRejectsUnparsedSet(t *testing.T) {
	f := newFixture(t)
	if _, err := Check(context.Background(), f.store, f.run.ID, Set{}); err == nil {
		t.Fatal("unparsed set evaluated")
	}
}

func TestParseRejectsInvalidAssertions(t *testing.T) {
	manifest := manifestFrom(t, "company")
	one := func(body string) string {
		return "version: 1\nassertions:\n  - " + strings.ReplaceAll(body, "\n", "\n    ") + "\n"
	}
	cases := map[string]string{
		"no assertions":           "version: 1\nassertions: []\n",
		"version two":             "version: 2\nassertions: [{id: a, type: event_absent, event: error}]\n",
		"string version":          "version: '1'\nassertions: [{id: a, type: event_absent, event: error}]\n",
		"multiple documents":      one("id: a\ntype: event_absent\nevent: error") + "---\nversion: 1\n",
		"unknown top field":       "version: 1\nsurprise: true\nassertions: [{id: a, type: event_absent, event: error}]\n",
		"unknown type":            one("id: a\ntype: sql\ntable: refunds"),
		"bad id":                  one("id: 'a b'\ntype: event_absent\nevent: error"),
		"missing id":              one("type: event_absent\nevent: error"),
		"null value":              one("id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents\nvalue: null"),
		"unknown table":           one("id: a\ntype: row_count\ntable: payroll\nequals: 0"),
		"unknown column":          one("id: a\ntype: row_count\ntable: refunds\nwhere: {secret: x}\nequals: 0"),
		"wrong value type":        one("id: a\ntype: row_count\ntable: refunds\nwhere: {amount_cents: '5'}\nequals: 0"),
		"contains on integer":     one("id: a\ntype: row_count\ntable: refunds\nwhere: {amount_cents: {contains: '5'}}\nequals: 0"),
		"unknown operator":        one("id: a\ntype: row_count\ntable: refunds\nwhere: {reason: {like: x}}\nequals: 0"),
		"no bound":                one("id: a\ntype: row_count\ntable: refunds"),
		"two bounds":              one("id: a\ntype: row_count\ntable: refunds\nequals: 1\nat_least: 1"),
		"negative bound":          one("id: a\ntype: row_count\ntable: refunds\nat_least: -1"),
		"bad entity":              one("id: a\ntype: field_equals\nentity: charges\nfield: refunded_cents\nvalue: 0"),
		"entity without id":       one("id: a\ntype: field_equals\nentity: message_members.x\nfield: channel_id\nvalue: x"),
		"value and value_from":    one("id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents\nvalue: 0\nvalue_from: charges.CH-1002.amount_cents"),
		"value_from type differs": one("id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents\nvalue_from: charges.CH-1002.invoice_id"),
		"missing value":           one("id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents"),
		"relationship no id":      one("id: a\ntype: relationship\ntable: refunds\nfield: charge_id\nreferences: message_members"),
		"unknown event":           one("id: a\ntype: event_exists\nevent: made.up"),
		"event_exists with bound": one("id: a\ntype: event_exists\nevent: error\nequals: 1"),
		"event with table":        one("id: a\ntype: event_absent\nevent: error\ntable: refunds"),
		"unknown service":         one("id: a\ntype: mutation_forbidden\nservice: payroll"),
		"service and operation":   one("id: a\ntype: mutation_forbidden\nservice: billing\noperation: createRefund"),
		"unknown operation":       one("id: a\ntype: mutation_forbidden\noperation: payEmployees"),
		"order missing then":      one("id: a\ntype: event_order\nfirst: {event: tool.request}"),
		"unknown custom":          one("id: a\ntype: custom\nname: missing"),
		"duplicate ids":           "version: 1\nassertions: [{id: a, type: event_absent, event: error}, {id: a, type: event_absent, event: retry}]\n",
		"empty ignored field":     one("id: a\ntype: event_exists\nevent: error\ntable: ''"),
		"empty value_from":        one("id: a\ntype: field_equals\nentity: charges.CH-1001\nfield: refunded_cents\nvalue: 0\nvalue_from: ''"),
		"empty service":           one("id: a\ntype: row_count\ntable: refunds\nequals: 0\nservice: \"\""),
	}
	for name, input := range cases {
		if _, err := Parse([]byte(input), manifest, testCustoms); err == nil {
			t.Errorf("%s: invalid assertions accepted", name)
		}
	}
	if _, err := Parse([]byte(one("id: a\ntype: mutation_forbidden\nservice: messaging")), manifestFrom(t, "billing"), testCustoms); err == nil {
		t.Error("service absent from the manifest accepted")
	}
}
