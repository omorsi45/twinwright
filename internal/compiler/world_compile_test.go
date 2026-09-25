package compiler

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"twinwright/internal/behavior"
)

const twoServiceWorld = `version: 1
name: sample
seed_profile: company-v1
services:
  - {name: billing, module: billing, openapi: billing.yaml, bindings: billing-bindings.yaml}
  - {name: crm, module: crm, openapi: crm.yaml, bindings: crm-bindings.yaml}
entities:
  - {service: billing, name: customers, primary_key: id, fields: [id]}
  - {service: crm, name: accounts, primary_key: id, fields: [id, customer_id]}
relationships:
  - {from: crm.accounts.customer_id, to: billing.customers.id}
`

func twoServiceFiles() map[string][]byte {
	return map[string][]byte{
		"billing.yaml": []byte(`openapi: 3.1.0
paths:
  /customers/{id}:
    get:
      operationId: getCustomer
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
`),
		"billing-bindings.yaml": []byte("operations:\n  getCustomer: billing.getCustomer\n"),
		"crm.yaml": []byte(`openapi: 3.1.0
paths:
  /crm/accounts/{id}:
    get:
      operationId: crmGetAccount
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
`),
		"crm-bindings.yaml": []byte("operations:\n  crmGetAccount: crm.getAccount\n"),
	}
}

func TestCompileWorldSupportsParameterlessGet(t *testing.T) {
	definition := `version: 1
name: health-world
seed_profile: company-v1
services:
  - {name: status, module: status, openapi: status.yaml, bindings: status-bindings.yaml}
`
	files := map[string][]byte{
		"status.yaml":          []byte("openapi: 3.1.0\npaths:\n  /health:\n    get:\n      operationId: getHealth\n"),
		"status-bindings.yaml": []byte("operations:\n  getHealth: status.health\n"),
	}
	registry := behavior.Builtin()
	if err := registry.Register("status.health", func(context.Context, *sql.Tx, string, string, string, string, map[string]any) (int, any, any, error) {
		return 200, map[string]string{"status": "ok"}, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := CompileWorld([]byte(definition), func(path string) ([]byte, error) { return files[path], nil }, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Operations) != 1 || len(manifest.Operations[0].Properties) != 0 {
		t.Fatalf("GET operation=%+v", manifest.Operations)
	}
}

func compileTestWorld(t *testing.T, definition string, files map[string][]byte) (Manifest, error) {
	t.Helper()
	return CompileWorld([]byte(definition), func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			t.Fatalf("unexpected load %q", path)
		}
		return data, nil
	}, behavior.Builtin())
}

func TestCompileWorldCombinesServiceManifests(t *testing.T) {
	manifest, err := compileTestWorld(t, twoServiceWorld, twoServiceFiles())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.World == nil || len(manifest.World.Services) != 2 || len(manifest.Operations) != 2 {
		t.Fatalf("world manifest=%+v", manifest)
	}
	services := map[string]string{}
	for _, op := range manifest.Operations {
		services[op.ID] = op.Service
	}
	if services["getCustomer"] != "billing" || services["crmGetAccount"] != "crm" {
		t.Fatalf("service namespaces=%+v", manifest.Operations)
	}
	if err := ValidateManifestWithRegistry(manifest, behavior.Builtin()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("default validation: %v", err)
	}
	reordered := strings.Replace(twoServiceWorld,
		"  - {name: billing, module: billing, openapi: billing.yaml, bindings: billing-bindings.yaml}\n  - {name: crm, module: crm, openapi: crm.yaml, bindings: crm-bindings.yaml}",
		"  - {name: crm, module: crm, openapi: crm.yaml, bindings: crm-bindings.yaml}\n  - {name: billing, module: billing, openapi: billing.yaml, bindings: billing-bindings.yaml}", 1)
	other, err := compileTestWorld(t, reordered, twoServiceFiles())
	if err != nil || manifest.Digest != other.Digest {
		t.Fatalf("reordered world digest=%s, original=%s, error=%v", other.Digest, manifest.Digest, err)
	}
	changed := strings.Replace(twoServiceWorld, "to: billing.customers.id", "to: crm.accounts.id", 1)
	other, err = compileTestWorld(t, changed, twoServiceFiles())
	if err != nil || manifest.Digest == other.Digest {
		t.Fatalf("relationship change digest=%s, original=%s, error=%v", other.Digest, manifest.Digest, err)
	}
}

func TestCompileWorldRejectsUnsupportedContracts(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string][]byte) string
	}{
		{"unknown behavior", func(f map[string][]byte) string {
			f["crm-bindings.yaml"] = []byte("operations:\n  crmGetAccount: crm.notRegistered\n")
			return twoServiceWorld
		}},
		{"wrong behavior module", func(f map[string][]byte) string {
			f["crm-bindings.yaml"] = []byte("operations:\n  crmGetAccount: billing.getCustomer\n")
			return twoServiceWorld
		}},
		{"duplicate operation ID", func(f map[string][]byte) string {
			f["crm.yaml"] = []byte(strings.Replace(string(f["crm.yaml"]), "crmGetAccount", "getCustomer", 1))
			f["crm-bindings.yaml"] = []byte("operations:\n  getCustomer: crm.getAccount\n")
			return twoServiceWorld
		}},
		{"unsupported method", func(f map[string][]byte) string {
			f["crm.yaml"] = []byte(strings.Replace(string(f["crm.yaml"]), "    get:", "    patch:", 1))
			return twoServiceWorld
		}},
		{"unsupported schema", func(f map[string][]byte) string {
			f["crm.yaml"] = []byte(strings.Replace(string(f["crm.yaml"]), "type: string", "type: array", 1))
			return twoServiceWorld
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := twoServiceFiles()
			definition := tc.change(files)
			if _, err := compileTestWorld(t, definition, files); err == nil {
				t.Fatal("unsupported contract accepted")
			}
		})
	}
}
