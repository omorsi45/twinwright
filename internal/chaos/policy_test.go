package chaos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/compiler"
)

func billingManifest(t *testing.T) compiler.Manifest {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "billing")
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

func TestParseSupportsEveryChaosType(t *testing.T) {
	manifest := billingManifest(t)
	cases := []struct{ name, rule string }{
		{"http", "type: http_error\n    operations: [listCharges]\n    status: 503\n    times: 1"},
		{"latency", "type: latency\n    operations: [listCharges]\n    duration_ms: 5000"},
		{"timeout", "type: timeout\n    operations: [listCharges]\n    times: 1"},
		{"committed timeout", "type: timeout_after_commit\n    operations: [createRefund]\n    times: 1"},
		{"rate limit", "type: rate_limit\n    operations: [listCharges]\n    after_calls: 3"},
		{"stale read", "type: stale_read\n    operations: [getCharge]\n    after_calls: 1"},
		{"malformed", "type: malformed_response\n    operations: [getCharge]\n    body: '{broken'\n    times: 1"},
		{"permission", "type: permission_revocation\n    operations: [getCharge]\n    after_calls: 1"},
		{"actor", "type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: createRefund\n      arguments: {charge_id: CH-1002, amount_cents: 500, reason: actor}"},
		{"outage", "type: partial_service_outage\n    operations: [getCharge, listCharges]\n    times: 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := Parse([]byte("version: 1\nrules:\n  - id: rule-1\n    "+tc.rule+"\n"), manifest)
			if err != nil {
				t.Fatal(err)
			}
			if policy.Version != 1 || len(policy.Rules) != 1 || policy.Digest() == "" {
				t.Fatalf("policy=%+v", policy)
			}
		})
	}
}

func TestPolicyDigestIsStableAndOrderSensitive(t *testing.T) {
	manifest := billingManifest(t)
	left := []byte("version: 1\nrules:\n  - {id: first, type: timeout, operations: [getCharge], times: 1}\n  - {id: second, type: latency, operations: [listCharges], duration_ms: 10}\n")
	a, err := Parse(left, manifest)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(left, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Fatal("same policy has different digest")
	}
	reversed := []byte("version: 1\nrules:\n  - {id: second, type: latency, operations: [listCharges], duration_ms: 10}\n  - {id: first, type: timeout, operations: [getCharge], times: 1}\n")
	c, err := Parse(reversed, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest() == a.Digest() {
		t.Fatal("rule order did not affect digest")
	}
	encoded, err := a.CanonicalJSON()
	if err != nil || !strings.Contains(string(encoded), "first") {
		t.Fatalf("canonical=%s err=%v", encoded, err)
	}
}

func TestParseRejectsInvalidPolicies(t *testing.T) {
	manifest := billingManifest(t)
	cases := []string{
		"version: 1\nrules:\n  - {id: x, type: http_error, operations: [missing], status: 503, times: 1}",
		"version: 1\nrules:\n  - {id: x, type: http_error, operations: [listCharges], status: 503, times: -1}",
		"version: 1\nrules:\n  - {id: x, type: latency, operations: [getCharge], duration_ms: -1}",
		"version: 1\nrules:\n  - {id: x, type: timeout_after_commit, operations: [getCharge], times: 1}",
		"version: 1\nrules:\n  - {id: x, type: stale_read, operations: [createRefund], after_calls: 1}",
		"version: 1\nrules:\n  - {id: x, type: concurrent_mutation, operations: [getCharge], times: 1, actor: {operation: createRefund, arguments: {charge_id: CH-1002}}}",
		"version: 1\nrules:\n  - {id: x, type: timeout, operations: [getCharge], times: 1, surprise: yes}",
		"version: 1\nrules:\n  - {id: x, type: timeout, operations: [getCharge], times: 1}\n  - {id: x, type: timeout, operations: [getCharge], times: 1}",
		"version: 1.5\nrules:\n  - {id: x, type: timeout, operations: [getCharge], times: 1}",
		"version: 1\nrules:\n  - {id: x, type: timeout, operations: [getCharge], times: 1}\n---\nversion: 1",
	}
	for i, input := range cases {
		if _, err := Parse([]byte(input), manifest); err == nil {
			t.Errorf("invalid case %d accepted: %s", i, input)
		}
	}
}

func TestParseUsesBehaviorSemanticsForSearchAndActor(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "company")
	spec, _ := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	bindings, _ := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		"type: stale_read\n    operations: [crmSearchAccounts]\n    after_calls: 1",
		"type: timeout\n    operations: [crmSearchAccounts]\n    times: 1\n    after_calls: 2",
		"type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: ticketTransitionIssue\n      arguments: {issue_id: ISSUE-1, status: in_progress}",
	} {
		if _, err := Parse([]byte("version: 1\nrules:\n  - id: x\n    "+rule+"\n"), manifest); err != nil {
			t.Fatalf("valid rule rejected: %v", err)
		}
	}
	for _, rule := range []string{
		"type: timeout_after_commit\n    operations: [crmSearchAccounts]\n    times: 1",
		"type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: crmSearchAccounts\n      arguments: {query: Morgan}",
		"type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: crmUpdateAccountStatus\n      arguments: {account_id: A-104, status: bogus}",
		"type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: ticketTransitionIssue\n      arguments: {issue_id: ISSUE-1, status: investigating}",
	} {
		if _, err := Parse([]byte("version: 1\nrules:\n  - id: x\n    "+rule+"\n"), manifest); err == nil {
			t.Fatalf("invalid rule accepted: %s", rule)
		}
	}
	if _, err := Parse([]byte("version: 1\nrules:\n  - id: x\n    type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: createRefund\n      arguments: {charge_id: CH-1002, amount_cents: -1, reason: actor}\n"), manifest); err == nil {
		t.Fatal("negative actor refund accepted")
	}
}
