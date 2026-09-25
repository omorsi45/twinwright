package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the executable surface. Behavior is selected by an explicit binding.
type Manifest struct {
	Digest     string      `json:"digest"`
	Operations []Operation `json:"operations"`
}

type Operation struct {
	ID         string            `json:"id"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Behavior   string            `json:"behavior"`
	Required   []string          `json:"required"`
	Properties map[string]string `json:"properties"`
}

func (m Manifest) Operation(id string) *Operation {
	for i := range m.Operations {
		if m.Operations[i].ID == id {
			return &m.Operations[i]
		}
	}
	return nil
}

type apiSpec struct {
	OpenAPI string                             `yaml:"openapi"`
	Paths   map[string]map[string]apiOperation `yaml:"paths"`
}
type apiOperation struct {
	ID         string `yaml:"operationId"`
	Parameters []struct {
		Name     string `yaml:"name"`
		In       string `yaml:"in"`
		Required bool   `yaml:"required"`
		Schema   struct {
			Type string `yaml:"type"`
		} `yaml:"schema"`
	} `yaml:"parameters"`
	RequestBody *struct {
		Required bool `yaml:"required"`
		Content  map[string]struct {
			Schema struct {
				Type       string   `yaml:"type"`
				Required   []string `yaml:"required"`
				Properties map[string]struct {
					Type string `yaml:"type"`
				} `yaml:"properties"`
			} `yaml:"schema"`
		} `yaml:"content"`
	} `yaml:"requestBody"`
}

func Compile(specBytes, bindingBytes []byte) (Manifest, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(specBytes, &node); err != nil {
		return Manifest{}, fmt.Errorf("OpenAPI YAML: %w", err)
	}
	if hasRef(&node) {
		return Manifest{}, fmt.Errorf("external or local reference is unsupported")
	}
	if err := checkSchemas(&node); err != nil {
		return Manifest{}, err
	}
	var spec apiSpec
	if err := node.Decode(&spec); err != nil {
		return Manifest{}, fmt.Errorf("OpenAPI: %w", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.1.") || len(spec.Paths) == 0 {
		return Manifest{}, fmt.Errorf("unsupported OpenAPI version or empty paths")
	}
	var bindings struct {
		Operations map[string]string `yaml:"operations"`
	}
	if err := yaml.Unmarshal(bindingBytes, &bindings); err != nil {
		return Manifest{}, fmt.Errorf("bindings YAML: %w", err)
	}
	m := Manifest{}
	counts := map[string]int{}
	for _, methods := range spec.Paths {
		for _, def := range methods {
			if def.ID != "" {
				counts[def.ID]++
			}
		}
	}
	for id, count := range counts {
		if count > 1 {
			return Manifest{}, fmt.Errorf("duplicate operationId %s", id)
		}
	}
	seen := map[string]bool{}
	for path, methods := range spec.Paths {
		if !strings.HasPrefix(path, "/") {
			return Manifest{}, fmt.Errorf("unsupported path %q", path)
		}
		for method, def := range methods {
			if method != "get" && method != "post" {
				return Manifest{}, fmt.Errorf("unsupported method %s %s", method, path)
			}
			if def.ID == "" {
				return Manifest{}, fmt.Errorf("missing operationId at %s %s", method, path)
			}
			if seen[def.ID] {
				return Manifest{}, fmt.Errorf("duplicate operationId %s", def.ID)
			}
			seen[def.ID] = true
			route, known := routes[def.ID]
			if !known || route.method != method || route.path != path {
				return Manifest{}, fmt.Errorf("unsupported path or method for %s", def.ID)
			}
			behavior, ok := bindings.Operations[def.ID]
			if !ok || behavior == "" {
				return Manifest{}, fmt.Errorf("unbound operation %s", def.ID)
			}
			if behavior != route.behavior {
				return Manifest{}, fmt.Errorf("unsupported binding %s for %s", behavior, def.ID)
			}
			op := Operation{ID: def.ID, Method: strings.ToUpper(method), Path: path, Behavior: behavior, Properties: map[string]string{}}
			if method == "get" && def.RequestBody != nil {
				return Manifest{}, fmt.Errorf("unsupported request body in %s", def.ID)
			}
			for _, p := range def.Parameters {
				if p.In != "path" || !p.Required || p.Schema.Type != "string" {
					return Manifest{}, fmt.Errorf("unsupported parameter in %s", def.ID)
				}
				op.Required = append(op.Required, p.Name)
				op.Properties[p.Name] = "string"
			}
			if method == "post" {
				if def.RequestBody == nil || len(def.RequestBody.Content) != 1 {
					return Manifest{}, fmt.Errorf("unsupported request body in %s", def.ID)
				}
				body, ok := def.RequestBody.Content["application/json"]
				if !ok || !def.RequestBody.Required || body.Schema.Type != "object" {
					return Manifest{}, fmt.Errorf("unsupported request body in %s", def.ID)
				}
				for name, p := range body.Schema.Properties {
					if p.Type != "string" && p.Type != "integer" {
						return Manifest{}, fmt.Errorf("unsupported property %s in %s", name, def.ID)
					}
					op.Properties[name] = p.Type
				}
				op.Required = append(op.Required, body.Schema.Required...)
			}
			for _, name := range op.Required {
				if op.Properties[name] == "" {
					return Manifest{}, fmt.Errorf("required property %s missing schema in %s", name, def.ID)
				}
			}
			sort.Strings(op.Required)
			if err := validateContract(op); err != nil {
				return Manifest{}, err
			}
			m.Operations = append(m.Operations, op)
		}
	}
	for id := range bindings.Operations {
		if !seen[id] {
			return Manifest{}, fmt.Errorf("binding for nonexistent operation %s", id)
		}
	}
	sort.Slice(m.Operations, func(i, j int) bool { return m.Operations[i].ID < m.Operations[j].ID })
	m.Digest, _ = digestOperations(m.Operations)
	return m, nil
}

func hasRef(node *yaml.Node) bool {
	if node.Value == "$ref" {
		return true
	}
	for _, child := range node.Content {
		if hasRef(child) {
			return true
		}
	}
	return false
}

type contract struct {
	method, path, behavior string
	properties             map[string]string
}

var routes = map[string]contract{
	"getCustomer":            {"get", "/customers/{id}", "billing.getCustomer", map[string]string{"id": "string"}},
	"listInvoices":           {"get", "/customers/{id}/invoices", "billing.listInvoices", map[string]string{"id": "string"}},
	"listCharges":            {"get", "/invoices/{id}/charges", "billing.listCharges", map[string]string{"id": "string"}},
	"getCharge":              {"get", "/charges/{id}", "billing.getCharge", map[string]string{"id": "string"}},
	"createRefund":           {"post", "/refunds", "billing.createRefund", map[string]string{"charge_id": "string", "amount_cents": "integer", "reason": "string"}},
	"getSubscription":        {"get", "/subscriptions/{id}", "billing.getSubscription", map[string]string{"id": "string"}},
	"crmGetAccount":          {"get", "/crm/accounts/{id}", "crm.getAccount", map[string]string{"id": "string"}},
	"crmSearchAccounts":      {"post", "/crm/accounts/search", "crm.searchAccounts", map[string]string{"query": "string"}},
	"crmAddAccountNote":      {"post", "/crm/notes", "crm.addAccountNote", map[string]string{"account_id": "string", "body": "string"}},
	"crmUpdateAccountStatus": {"post", "/crm/accounts/status", "crm.updateAccountStatus", map[string]string{"account_id": "string", "status": "string"}},
	"ticketCreateIssue":      {"post", "/tickets/issues", "ticket.createIssue", map[string]string{"project_id": "string", "account_id": "string", "title": "string", "priority": "string"}},
	"ticketGetIssue":         {"get", "/tickets/issues/{id}", "ticket.getIssue", map[string]string{"id": "string"}},
	"ticketSearchIssues":     {"post", "/tickets/issues/search", "ticket.searchIssues", map[string]string{"query": "string"}},
	"ticketAddComment":       {"post", "/tickets/comments", "ticket.addComment", map[string]string{"issue_id": "string", "body": "string"}},
	"ticketTransitionIssue":  {"post", "/tickets/issues/transition", "ticket.transitionIssue", map[string]string{"issue_id": "string", "status": "string"}},
	"messageListChannels":    {"get", "/messages/workspaces/{id}/channels", "messaging.listChannels", map[string]string{"id": "string"}},
	"messageReadChannel":     {"get", "/messages/channels/{id}", "messaging.readChannel", map[string]string{"id": "string"}},
	"messagePostMessage":     {"post", "/messages/messages", "messaging.postMessage", map[string]string{"channel_id": "string", "body": "string"}},
}

func validateContract(op Operation) error {
	route, ok := routes[op.ID]
	if !ok || op.Method != strings.ToUpper(route.method) || op.Path != route.path || op.Behavior != route.behavior {
		return fmt.Errorf("unsupported path or binding for %s", op.ID)
	}
	expected := route.properties
	if len(op.Properties) != len(expected) || len(op.Required) != len(expected) {
		return fmt.Errorf("unsupported schema for %s", op.ID)
	}
	required := map[string]bool{}
	for _, name := range op.Required {
		required[name] = true
	}
	for name, typ := range expected {
		if op.Properties[name] != typ || !required[name] {
			return fmt.Errorf("unsupported schema for %s", op.ID)
		}
	}
	return nil
}

func digestOperations(ops []Operation) (string, error) {
	raw, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// ValidateManifest rejects edited or incompatible build artifacts before execution.
func ValidateManifest(m Manifest) error {
	if len(m.Operations) == 0 {
		return fmt.Errorf("empty manifest")
	}
	last := ""
	for _, op := range m.Operations {
		if op.ID <= last {
			return fmt.Errorf("manifest operations are unsorted or duplicated")
		}
		if err := validateContract(op); err != nil {
			return err
		}
		last = op.ID
	}
	digest, err := digestOperations(m.Operations)
	if err != nil {
		return err
	}
	if digest != m.Digest {
		return fmt.Errorf("manifest digest mismatch")
	}
	return nil
}

func checkSchemas(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i].Value, node.Content[i+1]
			if key == "schema" {
				if err := checkSchema(value); err != nil {
					return err
				}
			} else if err := checkSchemas(value); err != nil {
				return err
			}
		}
	} else {
		for _, child := range node.Content {
			if err := checkSchemas(child); err != nil {
				return err
			}
		}
	}
	return nil
}
func checkSchema(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("unsupported schema shape")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "type":
			if value.Kind != yaml.ScalarNode {
				return fmt.Errorf("unsupported schema type")
			}
		case "required":
			if value.Kind != yaml.SequenceNode {
				return fmt.Errorf("unsupported schema required")
			}
		case "properties":
			if value.Kind != yaml.MappingNode {
				return fmt.Errorf("unsupported schema properties")
			}
			for j := 1; j < len(value.Content); j += 2 {
				if err := checkSchema(value.Content[j]); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported schema keyword %s", key)
		}
	}
	return nil
}
