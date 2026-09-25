package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/compiler"
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

const supportPolicy = `version: 1
principal:
  id: support-agent-1
  roles: [support]
roles:
  support:
    allow: [customers.read, charges.read]
permissions:
  allow: [refunds.create]
resources:
  customer_ids: [C-104]
  channel_ids: []
constraints:
  refund_max_cents: 500
temporary_grants:
  - {permission: jira.issues.create, starts_at_call: 2, ends_at_call: 3}
revocations:
  - {permission: refunds.create, after_call: 3}
`

func TestParsePrincipalPolicyAndPermissionRegistry(t *testing.T) {
	manifest := manifestFrom(t, "company")
	for _, operation := range manifest.Operations {
		if permission, ok := Permission(operation); !ok || permission == "" {
			t.Errorf("missing permission for %s", operation.Behavior)
		}
	}
	if _, ok := Permission(compiler.Operation{Behavior: "custom.unmapped"}); ok {
		t.Fatal("unmapped behavior received a permission")
	}
	policy, err := Parse([]byte(supportPolicy), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Version != 1 || policy.Principal.ID != "support-agent-1" || policy.Resources.ChannelIDs == nil || len(*policy.Resources.ChannelIDs) != 0 || policy.Resources.ProjectIDs != nil || policy.Digest() == "" {
		t.Fatalf("policy=%+v", policy)
	}
	if policy.Constraints.RefundMaxCents == nil || *policy.Constraints.RefundMaxCents != 500 {
		t.Fatalf("constraints=%+v", policy.Constraints)
	}
	again, err := Parse([]byte(supportPolicy), manifest)
	if err != nil || policy.Digest() != again.Digest() {
		t.Fatalf("digest not deterministic: %v", err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil || !strings.Contains(string(encoded), "support-agent-1") || !strings.Contains(string(encoded), `"channel_ids":[]`) {
		t.Fatalf("canonical=%s err=%v", encoded, err)
	}
	for _, change := range [][2]string{
		{"allow: [customers.read, charges.read]", "allow: [customers.read]"},
		{"allow: [refunds.create]", "allow: [invoices.read]"},
		{"customer_ids: [C-104]", "customer_ids: [C-205]"},
		{"ends_at_call: 3", "ends_at_call: 4"},
	} {
		changed, err := Parse([]byte(strings.Replace(supportPolicy, change[0], change[1], 1)), manifest)
		if err != nil {
			t.Fatalf("%s: %v", change[1], err)
		}
		if changed.Digest() == policy.Digest() {
			t.Errorf("changing %q to %q kept the digest", change[0], change[1])
		}
	}
}

func TestParseCanonicalizesSetOrder(t *testing.T) {
	manifest := manifestFrom(t, "company")
	first, err := Parse([]byte("version: 1\nprincipal: {id: support, roles: [a, b]}\nroles:\n  a: {allow: [charges.read, customers.read]}\n  b: {allow: [invoices.read]}\nresources: {customer_ids: [C-205, C-104]}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte("version: 1\nprincipal: {id: support, roles: [b, a]}\nroles:\n  b: {allow: [invoices.read]}\n  a: {allow: [customers.read, charges.read]}\nresources: {customer_ids: [C-104, C-205]}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest() != second.Digest() {
		t.Fatal("equivalent set orders produced different digests")
	}
}

func TestParseRejectsInvalidPrincipalPolicies(t *testing.T) {
	company := manifestFrom(t, "company")
	cases := map[string]string{
		"empty principal":         "version: 1\nprincipal: {id: '', roles: []}\npermissions: {allow: [customers.read]}",
		"missing role":            "version: 1\nprincipal: {id: support, roles: [missing]}\npermissions: {allow: [customers.read]}",
		"duplicate role":          "version: 1\nprincipal: {id: support, roles: [a, a]}\nroles: {a: {allow: [customers.read]}}",
		"unknown permission":      "version: 1\nprincipal: {id: support}\npermissions: {allow: [unknown.read]}",
		"unknown role permission": "version: 1\nprincipal: {id: support, roles: [a]}\nroles: {a: {allow: [unknown.read]}}",
		"duplicate permission":    "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read, customers.read]}",
		"unknown field":           "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read]}\nsurprise: true",
		"string version":          "version: '1'\nprincipal: {id: support}",
		"version two":             "version: 2\nprincipal: {id: support}",
		"negative refund cap":     "version: 1\nprincipal: {id: support}\nconstraints: {refund_max_cents: -1}",
		"empty resource ID":       "version: 1\nprincipal: {id: support}\nresources: {customer_ids: ['']}",
		"duplicate resource ID":   "version: 1\nprincipal: {id: support}\nresources: {customer_ids: [C-104, C-104]}",
		"reversed grant window":   "version: 1\nprincipal: {id: support}\ntemporary_grants: [{permission: refunds.create, starts_at_call: 3, ends_at_call: 2}]",
		"grant at call zero":      "version: 1\nprincipal: {id: support}\ntemporary_grants: [{permission: refunds.create, starts_at_call: 0, ends_at_call: 2}]",
		"grant missing end":       "version: 1\nprincipal: {id: support}\ntemporary_grants: [{permission: refunds.create, starts_at_call: 1}]",
		"overlapping grants":      "version: 1\nprincipal: {id: support}\ntemporary_grants: [{permission: refunds.create, starts_at_call: 1, ends_at_call: 3}, {permission: refunds.create, starts_at_call: 3, ends_at_call: 5}]",
		"unknown grant":           "version: 1\nprincipal: {id: support}\ntemporary_grants: [{permission: unknown.read, starts_at_call: 1, ends_at_call: 2}]",
		"negative revocation":     "version: 1\nprincipal: {id: support}\nrevocations: [{permission: refunds.create, after_call: -1}]",
		"revocation missing call": "version: 1\nprincipal: {id: support}\nrevocations: [{permission: refunds.create}]",
		"duplicate revocation":    "version: 1\nprincipal: {id: support}\nrevocations: [{permission: refunds.create, after_call: 1}, {permission: refunds.create, after_call: 2}]",
		"multiple documents":      "version: 1\nprincipal: {id: support}\n---\nversion: 1",
		"not a mapping":           "- version: 1",
	}
	for name, input := range cases {
		if _, err := Parse([]byte(input), company); err == nil {
			t.Errorf("%s: invalid policy accepted", name)
		}
	}
	billing := manifestFrom(t, "billing")
	if _, err := Parse([]byte("version: 1\nprincipal: {id: support}\npermissions: {allow: [slack.messages.write]}"), billing); err == nil {
		t.Error("permission without an operation in the world accepted")
	}
}

func TestSupportPolicyExampleParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "security", "support-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := Parse(raw, manifestFrom(t, "company"))
	if err != nil {
		t.Fatal(err)
	}
	if policy.Principal.ID == "" || policy.Resources.CustomerIDs == nil {
		t.Fatalf("policy=%+v", policy)
	}
}
