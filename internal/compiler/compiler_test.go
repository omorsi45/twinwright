package compiler

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func example(t *testing.T) ([]byte, []byte) {
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
	return []byte(strings.ReplaceAll(string(spec), "\r\n", "\n")), bindings
}

func TestCompileExampleAndStableDigest(t *testing.T) {
	spec, bindings := example(t)
	a, err := Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Operations) != 5 || a.Digest != b.Digest {
		t.Fatalf("operations=%d digests=%s,%s", len(a.Operations), a.Digest, b.Digest)
	}
	if a.Digest != "1e90724d491f5de49a1271faf2136ddaf214773cfc8f0e5bd82f41b430c6e340" {
		t.Fatalf("billing digest changed: %s", a.Digest)
	}
	op := a.Operation("createRefund")
	if op == nil || op.Method != "POST" || len(op.Required) != 3 {
		t.Fatalf("refund operation=%+v", op)
	}
}

func TestCompileRejectsUnboundUnsupportedAndExternalReferences(t *testing.T) {
	spec, bindings := example(t)
	cases := []struct {
		name           string
		spec, bindings []byte
		want           string
	}{
		{"unbound", spec, []byte("operations:\n  getCustomer: billing.getCustomer\n"), "unbound"},
		{"unsupported method", []byte(strings.Replace(string(spec), "    post:\n", "    patch:\n", 1)), bindings, "unsupported"},
		{"external ref", append(append([]byte{}, spec...), []byte("\ncomponents:\n  schemas:\n    X:\n      $ref: https://example.com/schema.yaml\n")...), bindings, "reference"},
		{"duplicate id", []byte(strings.Replace(string(spec), "operationId: getCharge", "operationId: getCustomer", 1)), bindings, "duplicate"},
		{"wrong route", []byte(strings.Replace(string(spec), "/refunds:", "/unrelated:", 1)), bindings, "unsupported path"},
		{"optional reason", []byte(strings.Replace(string(spec), "required: [charge_id, amount_cents, reason]", "required: [charge_id, amount_cents]", 1)), bindings, "unsupported schema"},
		{"wrong reason type", []byte(strings.Replace(string(spec), "reason: {type: string}", "reason: {type: integer}", 1)), bindings, "unsupported schema"},
		{"missing path argument", []byte(strings.Replace(string(spec), "        - name: id\n          in: path\n          required: true\n          schema: {type: string}\n", "", 1)), bindings, "unsupported schema"},
		{"ignored enum", []byte(strings.Replace(string(spec), "reason: {type: string}", "reason: {type: string, enum: [duplicate]}", 1)), bindings, "unsupported schema"},
		{"ignored minimum", []byte(strings.Replace(string(spec), "amount_cents: {type: integer}", "amount_cents: {type: integer, minimum: 1}", 1)), bindings, "unsupported schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.spec, tc.bindings)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %s", err, tc.want)
			}
		})
	}
}

func TestCompileCompanyContracts(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "company")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Operations) != 18 {
		t.Fatalf("operations=%d, want 18", len(m.Operations))
	}
	if err := ValidateManifest(m); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, method, path, behavior string
		props                      map[string]string
	}{
		{"getSubscription", "GET", "/subscriptions/{id}", "billing.getSubscription", map[string]string{"id": "string"}},
		{"crmGetAccount", "GET", "/crm/accounts/{id}", "crm.getAccount", map[string]string{"id": "string"}},
		{"crmSearchAccounts", "POST", "/crm/accounts/search", "crm.searchAccounts", map[string]string{"query": "string"}},
		{"crmAddAccountNote", "POST", "/crm/notes", "crm.addAccountNote", map[string]string{"account_id": "string", "body": "string"}},
		{"crmUpdateAccountStatus", "POST", "/crm/accounts/status", "crm.updateAccountStatus", map[string]string{"account_id": "string", "status": "string"}},
		{"ticketCreateIssue", "POST", "/tickets/issues", "ticket.createIssue", map[string]string{"project_id": "string", "account_id": "string", "title": "string", "priority": "string"}},
		{"ticketGetIssue", "GET", "/tickets/issues/{id}", "ticket.getIssue", map[string]string{"id": "string"}},
		{"ticketSearchIssues", "POST", "/tickets/issues/search", "ticket.searchIssues", map[string]string{"query": "string"}},
		{"ticketAddComment", "POST", "/tickets/comments", "ticket.addComment", map[string]string{"issue_id": "string", "body": "string"}},
		{"ticketTransitionIssue", "POST", "/tickets/issues/transition", "ticket.transitionIssue", map[string]string{"issue_id": "string", "status": "string"}},
		{"messageListChannels", "GET", "/messages/workspaces/{id}/channels", "messaging.listChannels", map[string]string{"id": "string"}},
		{"messageReadChannel", "GET", "/messages/channels/{id}", "messaging.readChannel", map[string]string{"id": "string"}},
		{"messagePostMessage", "POST", "/messages/messages", "messaging.postMessage", map[string]string{"channel_id": "string", "body": "string"}},
	} {
		op := m.Operation(tc.id)
		if op == nil || op.Method != tc.method || op.Path != tc.path || op.Behavior != tc.behavior || !reflect.DeepEqual(op.Properties, tc.props) {
			t.Fatalf("operation %s=%+v, want %s %s %s %v", tc.id, op, tc.method, tc.path, tc.behavior, tc.props)
		}
		if len(op.Required) != len(tc.props) {
			t.Fatalf("required for %s=%v", tc.id, op.Required)
		}
		for _, name := range op.Required {
			if tc.props[name] == "" {
				t.Fatalf("unexpected required %s in %s", name, tc.id)
			}
		}
	}
}

func TestCompileCompanyRejectsInvalidContracts(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "company")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, new, want string
		binding              bool
	}{
		{"unknown binding", "crm.getAccount", "crm.unknown", "unsupported binding", true},
		{"wrong route", "/crm/accounts/{id}:", "/crm/customers/{id}:", "unsupported path", false},
		{"wrong required", "required: [account_id, body]", "required: [account_id]", "unsupported schema", false},
		{"wrong property type", "body: {type: string}", "body: {type: integer}", "unsupported schema", false},
		{"duplicate operation ID", "operationId: ticketGetIssue", "operationId: crmGetAccount", "duplicate", false},
		{"GET with request body", "operationId: crmGetAccount, parameters:", "operationId: crmGetAccount, requestBody: {required: true, content: {application/json: {schema: {type: object, required: [id], properties: {id: {type: string}}}}}}, parameters:", "unsupported request body", false},
		{"extra content type", "application/json: {schema: {type: object, required: [query], properties: {query: {type: string}}}}", "application/json: {schema: {type: object, required: [query], properties: {query: {type: string}}}}, text/plain: {schema: {type: string}}", "unsupported request body", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b := string(spec), string(bindings)
			if tc.binding {
				b = strings.Replace(b, tc.old, tc.new, 1)
			} else {
				s = strings.Replace(s, tc.old, tc.new, 1)
			}
			if (tc.binding && b == string(bindings)) || (!tc.binding && s == string(spec)) {
				t.Fatalf("fixture missing %q", tc.old)
			}
			_, err := Compile([]byte(s), []byte(b))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %s", err, tc.want)
			}
		})
	}
}
