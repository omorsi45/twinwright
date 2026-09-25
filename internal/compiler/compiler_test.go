package compiler

import (
	"os"
	"path/filepath"
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
	return spec, bindings
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
