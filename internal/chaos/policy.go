package chaos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
	"twinwright/internal/compiler"
)

// Policy is the ordered, canonical rule set stored with a run.
type Policy struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

type Rule struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Operations []string `json:"operations"`
	Status     int      `json:"status,omitempty"`
	Times      int      `json:"times,omitempty"`
	AfterCalls int      `json:"after_calls,omitempty"`
	DurationMS int      `json:"duration_ms,omitempty"`
	Body       string   `json:"body,omitempty"`
	Actor      *Actor   `json:"actor,omitempty"`
}

type Actor struct {
	Operation string         `json:"operation"`
	Arguments map[string]any `json:"arguments"`
}

type rawPolicy struct {
	Version int       `yaml:"version"`
	Rules   []rawRule `yaml:"rules"`
}

type rawRule struct {
	ID         string   `yaml:"id"`
	Type       string   `yaml:"type"`
	Operations []string `yaml:"operations"`
	Status     *int     `yaml:"status"`
	Times      *int     `yaml:"times"`
	AfterCalls *int     `yaml:"after_calls"`
	DurationMS *int     `yaml:"duration_ms"`
	Body       *string  `yaml:"body"`
	Actor      *struct {
		Operation string         `yaml:"operation"`
		Arguments map[string]any `yaml:"arguments"`
	} `yaml:"actor"`
}

var ruleID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Parse rejects policy shapes that cannot be executed deterministically.
func Parse(raw []byte, manifest compiler.Manifest) (Policy, error) {
	if err := compiler.ValidateManifest(manifest); err != nil {
		return Policy{}, fmt.Errorf("manifest: %w", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Policy{}, fmt.Errorf("chaos policy YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Policy{}, fmt.Errorf("chaos policy must be a mapping")
	}
	for i := 0; i < len(node.Content[0].Content); i += 2 {
		if node.Content[0].Content[i].Value == "version" && node.Content[0].Content[i+1].Tag != "!!int" {
			return Policy{}, fmt.Errorf("chaos policy version must be an integer")
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input rawPolicy
	if err := decoder.Decode(&input); err != nil {
		return Policy{}, fmt.Errorf("chaos policy YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Policy{}, fmt.Errorf("chaos policy YAML: %w", err)
		}
		return Policy{}, fmt.Errorf("chaos policy has multiple YAML documents")
	}
	if input.Version != 1 {
		return Policy{}, fmt.Errorf("unsupported chaos policy version %d", input.Version)
	}
	if len(input.Rules) == 0 {
		return Policy{}, fmt.Errorf("chaos policy requires at least one rule")
	}
	policy := Policy{Version: 1, Rules: make([]Rule, 0, len(input.Rules))}
	ids := map[string]bool{}
	for _, entry := range input.Rules {
		if !ruleID.MatchString(entry.ID) || ids[entry.ID] {
			return Policy{}, fmt.Errorf("invalid or duplicate chaos rule ID %q", entry.ID)
		}
		ids[entry.ID] = true
		if len(entry.Operations) == 0 {
			return Policy{}, fmt.Errorf("rule %s requires operations", entry.ID)
		}
		opIDs := map[string]bool{}
		for _, id := range entry.Operations {
			if manifest.Operation(id) == nil || opIDs[id] {
				return Policy{}, fmt.Errorf("rule %s has unknown or duplicate operation %q", entry.ID, id)
			}
			opIDs[id] = true
		}
		if entry.Times != nil && *entry.Times < 1 {
			return Policy{}, fmt.Errorf("rule %s times must be positive", entry.ID)
		}
		if entry.AfterCalls != nil && *entry.AfterCalls < 0 {
			return Policy{}, fmt.Errorf("rule %s after_calls must be nonnegative", entry.ID)
		}
		if entry.DurationMS != nil && *entry.DurationMS < 1 {
			return Policy{}, fmt.Errorf("rule %s duration_ms must be positive", entry.ID)
		}
		rule := Rule{ID: entry.ID, Type: entry.Type, Operations: entry.Operations}
		if entry.Times != nil {
			rule.Times = *entry.Times
		}
		if entry.AfterCalls != nil {
			rule.AfterCalls = *entry.AfterCalls
		}
		switch entry.Type {
		case "http_error":
			if entry.Status == nil || *entry.Status < 400 || *entry.Status > 599 || entry.Times == nil {
				return Policy{}, fmt.Errorf("rule %s requires HTTP status 400-599 and positive times", entry.ID)
			}
			rule.Status = *entry.Status
		case "latency":
			if entry.DurationMS == nil {
				return Policy{}, fmt.Errorf("rule %s requires duration_ms", entry.ID)
			}
			rule.DurationMS = *entry.DurationMS
		case "timeout", "malformed_response", "concurrent_mutation", "partial_service_outage", "timeout_after_commit":
			if entry.Times == nil {
				return Policy{}, fmt.Errorf("rule %s requires times", entry.ID)
			}
		case "rate_limit", "permission_revocation":
			if entry.AfterCalls == nil {
				return Policy{}, fmt.Errorf("rule %s requires after_calls", entry.ID)
			}
		case "stale_read":
			if entry.AfterCalls == nil || *entry.AfterCalls < 1 {
				return Policy{}, fmt.Errorf("rule %s requires after_calls of at least one", entry.ID)
			}
		default:
			return Policy{}, fmt.Errorf("rule %s has unsupported type %q", entry.ID, entry.Type)
		}
		if entry.Type != "http_error" && entry.Status != nil {
			return Policy{}, fmt.Errorf("rule %s does not accept status", entry.ID)
		}
		if entry.Type != "latency" && entry.DurationMS != nil {
			return Policy{}, fmt.Errorf("rule %s does not accept duration_ms", entry.ID)
		}
		if entry.Type != "rate_limit" && entry.Type != "permission_revocation" && entry.Type != "stale_read" && entry.AfterCalls != nil {
			return Policy{}, fmt.Errorf("rule %s does not accept after_calls", entry.ID)
		}
		if entry.Type == "timeout_after_commit" || entry.Type == "stale_read" {
			for _, id := range entry.Operations {
				method := manifest.Operation(id).Method
				if entry.Type == "timeout_after_commit" && method != "POST" || entry.Type == "stale_read" && method != "GET" {
					return Policy{}, fmt.Errorf("rule %s cannot target %s operation %s", entry.ID, method, id)
				}
			}
		}
		if entry.Type == "partial_service_outage" && len(entry.Operations) < 2 {
			return Policy{}, fmt.Errorf("rule %s requires multiple operations", entry.ID)
		}
		if entry.Type == "malformed_response" {
			if entry.Body == nil || *entry.Body == "" {
				return Policy{}, fmt.Errorf("rule %s requires body", entry.ID)
			}
			rule.Body = *entry.Body
		} else if entry.Body != nil {
			return Policy{}, fmt.Errorf("rule %s does not accept body", entry.ID)
		}
		if entry.Type == "concurrent_mutation" {
			if entry.Actor == nil {
				return Policy{}, fmt.Errorf("rule %s requires actor", entry.ID)
			}
			op := manifest.Operation(entry.Actor.Operation)
			if op == nil || op.Method != "POST" {
				return Policy{}, fmt.Errorf("rule %s actor must use a compiled POST operation", entry.ID)
			}
			if err := validateArguments(*op, entry.Actor.Arguments); err != nil {
				return Policy{}, fmt.Errorf("rule %s actor: %w", entry.ID, err)
			}
			rule.Actor = &Actor{Operation: entry.Actor.Operation, Arguments: entry.Actor.Arguments}
		} else if entry.Actor != nil {
			return Policy{}, fmt.Errorf("rule %s does not accept actor", entry.ID)
		}
		policy.Rules = append(policy.Rules, rule)
	}
	if _, err := policy.CanonicalJSON(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func validateArguments(op compiler.Operation, args map[string]any) error {
	for _, name := range op.Required {
		if _, ok := args[name]; !ok {
			return fmt.Errorf("missing %s", name)
		}
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := args[name]
		typeName, ok := op.Properties[name]
		if !ok {
			return fmt.Errorf("unexpected %s", name)
		}
		switch typeName {
		case "string":
			if text, ok := value.(string); !ok || text == "" {
				return fmt.Errorf("invalid %s", name)
			}
		case "integer":
			if _, ok := value.(int); !ok {
				return fmt.Errorf("invalid %s", name)
			}
		default:
			return fmt.Errorf("unsupported argument type %s", typeName)
		}
	}
	return nil
}

func (p Policy) CanonicalJSON() ([]byte, error) { return json.Marshal(p) }

func (p Policy) Digest() string {
	encoded, err := p.CanonicalJSON()
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
