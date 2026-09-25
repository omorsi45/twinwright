package counterfactual

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

const everyKind = `version: 1
interventions:
  - id: latency-instead
    kind: chaos_policy
    calls: [refund-first]
    policy:
      version: 1
      rules:
        - {id: slow, type: latency, operations: [createRefund], duration_ms: 250}
  - id: refunds-only
    kind: auth_policy
    policy:
      version: 1
      principal: {id: refund-agent}
      permissions: {allow: [charges.read, refunds.create]}
  - id: charge-outage
    kind: fault
    operation: getCharge
  - id: no-fault
    kind: fault
  - id: safe-model
    kind: model
    provider: scripted
    model: fixture-safe-v1
  - id: delivered
    kind: tool_response
    call: refund-first
    status: 201
    body: {id: RF-1, charge_id: CH-1002, amount_cents: 500}
`

func TestParseAcceptsEveryKind(t *testing.T) {
	set, err := Parse([]byte(everyKind), billingManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(set.interventions) != 6 {
		t.Fatalf("interventions=%d", len(set.interventions))
	}
	delivered := set.interventions[5]
	if delivered.Call != "refund-first" || delivered.Status != 201 || string(delivered.Body) != `{"amount_cents":500,"charge_id":"CH-1002","id":"RF-1"}` {
		t.Fatalf("tool response intervention=%+v body=%s", delivered, delivered.Body)
	}
	if set.interventions[3].Operation != nil || set.interventions[2].Operation == nil || *set.interventions[2].Operation != "getCharge" {
		t.Fatalf("fault interventions=%+v %+v", set.interventions[2], set.interventions[3])
	}
	if len(set.interventions[0].policyRaw) == 0 || len(set.interventions[1].policyRaw) == 0 {
		t.Fatal("inline policies were not kept for the fork")
	}
}

func TestParseDigestIsStableAcrossKeyOrder(t *testing.T) {
	manifest := billingManifest(t)
	left, err := Parse([]byte("version: 1\ninterventions:\n  - {id: a, kind: tool_response, call: c1, status: 200, body: {x: 1, y: 2}}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	right, err := Parse([]byte("version: 1\ninterventions:\n  - {body: {y: 2, x: 1}, status: 200, call: c1, kind: tool_response, id: a}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := Parse([]byte("version: 1\ninterventions:\n  - {id: a, kind: tool_response, call: c1, status: 200, body: {x: 1, y: 3}}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest() == "" || left.Digest() != right.Digest() || left.Digest() == changed.Digest() {
		t.Fatalf("digests left=%s right=%s changed=%s", left.Digest(), right.Digest(), changed.Digest())
	}
}

func TestParseRejectsInvalidFiles(t *testing.T) {
	manifest := billingManifest(t)
	one := func(body string) string { return "version: 1\ninterventions:\n  - " + body + "\n" }
	cases := map[string]struct{ input, want string }{
		"not a mapping":         {"- 1", "must be a mapping"},
		"fractional version":    {"version: 1.5\ninterventions: []", "version must be an integer"},
		"unsupported version":   {"version: 2\ninterventions:\n  - {id: a, kind: fault}", "unsupported interventions version"},
		"multiple documents":    {one("{id: a, kind: fault}") + "---\nversion: 1\n", "multiple YAML documents"},
		"no interventions":      {"version: 1\ninterventions: []", "at least one intervention"},
		"invalid id":            {one("{id: 1a, kind: fault}"), "invalid or duplicate intervention id"},
		"duplicate id":          {one("{id: a, kind: fault}") + "  - {id: a, kind: fault}\n", "invalid or duplicate intervention id"},
		"unknown kind":          {one("{id: a, kind: memory}"), "unsupported kind"},
		"unknown field":         {one("{id: a, kind: fault, surprise: 1}"), "field surprise not found"},
		"field wrong for kind":  {one("{id: a, kind: fault, status: 200}"), "does not accept status"},
		"null value":            {one("{id: a, kind: fault, operation: null}"), "empty or null"},
		"empty string":          {one("{id: a, kind: model, model: ''}"), "empty or null"},
		"empty calls":           {one("{id: a, kind: fault, calls: []}"), "calls must not be empty"},
		"duplicate calls":       {one("{id: a, kind: fault, calls: [c1, c1]}"), "duplicate call"},
		"missing policy":        {one("{id: a, kind: chaos_policy}"), "requires policy"},
		"policy not mapping":    {one("{id: a, kind: chaos_policy, policy: 5}"), "policy must be a mapping"},
		"invalid chaos policy":  {one("{id: a, kind: chaos_policy, policy: {version: 1, rules: [{id: r, type: timeout, operations: [missing], times: 1}]}}"), "unknown or duplicate operation"},
		"invalid auth policy":   {one("{id: a, kind: auth_policy, policy: {version: 1, principal: {id: p}, permissions: {allow: [jira.issues.read]}}}"), "permission"},
		"unknown fault op":      {one("{id: a, kind: fault, operation: missing}"), "unknown fault operation"},
		"model without model":   {one("{id: a, kind: model, provider: scripted}"), "requires model"},
		"response without call": {one("{id: a, kind: tool_response, status: 200, body: {}}"), "requires call"},
		"response with calls":   {one("{id: a, kind: tool_response, calls: [c1], call: c1, status: 200, body: {}}"), "does not accept calls"},
		"response no status":    {one("{id: a, kind: tool_response, call: c1, body: {}}"), "requires status"},
		"response bad status":   {one("{id: a, kind: tool_response, call: c1, status: 700, body: {}}"), "status 700"},
		"response quoted":       {one("{id: a, kind: tool_response, call: c1, status: '200', body: {}}"), "cannot unmarshal"},
		"response no body":      {one("{id: a, kind: tool_response, call: c1, status: 200}"), "requires body"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.input), manifest)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.want)
		}
	}
}
