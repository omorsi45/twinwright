package compiler

import (
	"reflect"
	"strings"
	"testing"
)

const validWorldDefinition = `version: 1
name: support
seed_profile: company-v1
services:
  - name: ticketing
    module: ticket
    openapi: ticketing.yaml
    bindings: ticketing.bindings.yaml
  - name: crm
    module: crm
    openapi: crm.yaml
    bindings: crm.bindings.yaml
  - name: billing
    module: billing
    openapi: billing.yaml
    bindings: billing.bindings.yaml
entities:
  - service: ticketing
    name: issues
    primary_key: id
    fields: [status, account_id, id]
  - service: crm
    name: accounts
    primary_key: id
    fields: [status, customer_id, id]
  - service: billing
    name: customers
    primary_key: id
    fields: [name, id]
relationships:
  - from: ticketing.issues.account_id
    to: crm.accounts.id
  - from: crm.accounts.customer_id
    to: billing.customers.id
`

func TestWorldDefinitionParsesAndNormalizesMetadata(t *testing.T) {
	def, err := ParseWorldDefinition([]byte(validWorldDefinition))
	if err != nil {
		t.Fatal(err)
	}
	if def.Version != 1 || def.Name != "support" || def.SeedProfile != "company-v1" {
		t.Fatalf("metadata=%+v", def)
	}
	if got := []string{def.Services[0].Name, def.Services[1].Name, def.Services[2].Name}; !reflect.DeepEqual(got, []string{"billing", "crm", "ticketing"}) {
		t.Fatalf("service order=%v", got)
	}
	if got := []string{def.Entities[0].Service + "." + def.Entities[0].Name, def.Entities[1].Service + "." + def.Entities[1].Name, def.Entities[2].Service + "." + def.Entities[2].Name}; !reflect.DeepEqual(got, []string{"billing.customers", "crm.accounts", "ticketing.issues"}) {
		t.Fatalf("entity order=%v", got)
	}
	if !reflect.DeepEqual(def.Entities[1].Fields, []string{"customer_id", "id", "status"}) {
		t.Fatalf("fields=%v", def.Entities[1].Fields)
	}
	if got := []string{def.Relationships[0].From, def.Relationships[1].From}; !reflect.DeepEqual(got, []string{"crm.accounts.customer_id", "ticketing.issues.account_id"}) {
		t.Fatalf("relationship order=%v", got)
	}
}

func TestWorldDefinitionRejectsInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement, want string
	}{
		{"version", "version: 1", "version: 2", "version"},
		{"name", "name: support", `name: "  "`, "name"},
		{"seed profile", "seed_profile: company-v1", "seed_profile: unknown-v1", "seed profile"},
		{"empty services", "services:\n  - name: ticketing\n    module: ticket\n    openapi: ticketing.yaml\n    bindings: ticketing.bindings.yaml\n  - name: crm\n    module: crm\n    openapi: crm.yaml\n    bindings: crm.bindings.yaml\n  - name: billing\n    module: billing\n    openapi: billing.yaml\n    bindings: billing.bindings.yaml", "services: []", "service"},
		{"empty service name", "name: crm", `name: ""`, "service name"},
		{"duplicate service", "name: crm", "name: billing", "duplicate service"},
		{"missing service file", "openapi: crm.yaml", `openapi: ""`, "openapi"},
		{"unknown entity service", "service: ticketing", "service: unknown", "unknown service"},
		{"duplicate entity", "- service: billing\n    name: customers", "- service: crm\n    name: accounts", "duplicate entity"},
		{"missing primary key field", "primary_key: id", "primary_key: missing", "primary key"},
		{"duplicate field", "fields: [status, customer_id, id]", "fields: [status, id, id]", "duplicate field"},
		{"malformed from endpoint", "from: crm.accounts.customer_id", "from: crm.accounts", "relationship"},
		{"unknown from field", "from: crm.accounts.customer_id", "from: crm.accounts.missing", "unknown relationship"},
		{"unknown to field", "to: billing.customers.id", "to: billing.customers.missing", "unknown relationship"},
		{"duplicate relationship", "  - from: crm.accounts.customer_id\n    to: billing.customers.id", "  - from: crm.accounts.customer_id\n    to: billing.customers.id\n  - from: crm.accounts.customer_id\n    to: billing.customers.id", "duplicate relationship"},
		{"unknown YAML field", "name: support", "name: support\nunsupported: true", "field"},
		{"unknown service YAML field", "openapi: crm.yaml", "openapi: crm.yaml\n    unsupported: true", "field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(validWorldDefinition, tc.old, tc.replacement, 1)
			if input == validWorldDefinition {
				t.Fatalf("fixture missing %q", tc.old)
			}
			_, err := ParseWorldDefinition([]byte(input))
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestWorldDefinitionRejectsNonIdentifierNames(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement, want string }{
		{"service starts with digit", "name: crm", "name: 2crm", "service name"},
		{"service uses non ASCII", "name: crm", "name: crmé", "service name"},
		{"module contains space", "module: crm", "module: crm module", "module"},
		{"entity contains space", "name: accounts", "name: account names", "entity name"},
		{"field contains space", "fields: [status, customer_id, id]", "fields: [status, customer id, id]", "invalid field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(validWorldDefinition, tc.old, tc.replacement, 1)
			if input == validWorldDefinition {
				t.Fatalf("fixture missing %q", tc.old)
			}
			_, err := ParseWorldDefinition([]byte(input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}
