// Package counterfactual forks a failed run at candidate events, changes one
// controlled variable per fork, and reports how often the outcome changed.
package counterfactual

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"gopkg.in/yaml.v3"
	"twinwright/internal/authz"
	"twinwright/internal/chaos"
	"twinwright/internal/compiler"
)

const (
	KindChaosPolicy  = "chaos_policy"
	KindAuthPolicy   = "auth_policy"
	KindFault        = "fault"
	KindModel        = "model"
	KindToolResponse = "tool_response"
)

// Intervention is the canonical form of one controlled change.
type Intervention struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Calls     []string        `json:"calls,omitempty"`
	Policy    json.RawMessage `json:"policy,omitempty"`
	Operation *string         `json:"operation,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	Model     string          `json:"model,omitempty"`
	Call      string          `json:"call,omitempty"`
	Status    int             `json:"status,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	policyRaw []byte
}

// Set is a validated intervention file bound to its manifest. Only Parse can
// populate it.
type Set struct {
	interventions []Intervention
	manifest      compiler.Manifest
}

type rawIntervention struct {
	ID        string    `yaml:"id"`
	Kind      string    `yaml:"kind"`
	Calls     []string  `yaml:"calls"`
	Policy    yaml.Node `yaml:"policy"`
	Operation *string   `yaml:"operation"`
	Provider  string    `yaml:"provider"`
	Model     string    `yaml:"model"`
	Call      string    `yaml:"call"`
	Status    *int      `yaml:"status"`
	Body      yaml.Node `yaml:"body"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// fields lists which raw fields each kind may set; anything else is rejected.
var fields = map[string][]string{
	KindChaosPolicy:  {"calls", "policy"},
	KindAuthPolicy:   {"calls", "policy"},
	KindFault:        {"calls", "operation"},
	KindModel:        {"calls", "provider", "model"},
	KindToolResponse: {"call", "status", "body"},
}

// Parse validates a version 1 intervention file against a manifest.
func Parse(raw []byte, manifest compiler.Manifest) (Set, error) {
	if err := compiler.ValidateManifest(manifest); err != nil {
		return Set{}, fmt.Errorf("manifest: %w", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Set{}, fmt.Errorf("interventions YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Set{}, fmt.Errorf("interventions file must be a mapping")
	}
	root := node.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "version" && root.Content[i+1].Tag != "!!int" {
			return Set{}, fmt.Errorf("interventions version must be an integer")
		}
	}
	if err := rejectNulls(root, "interventions file"); err != nil {
		return Set{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input struct {
		Version       int               `yaml:"version"`
		Interventions []rawIntervention `yaml:"interventions"`
	}
	if err := decoder.Decode(&input); err != nil {
		return Set{}, fmt.Errorf("interventions YAML: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err != nil {
			return Set{}, fmt.Errorf("interventions YAML: %w", err)
		}
		return Set{}, fmt.Errorf("interventions file has multiple YAML documents")
	}
	if input.Version != 1 {
		return Set{}, fmt.Errorf("unsupported interventions version %d", input.Version)
	}
	if len(input.Interventions) == 0 {
		return Set{}, fmt.Errorf("interventions file requires at least one intervention")
	}
	set := Set{manifest: manifest}
	seen := map[string]bool{}
	for _, in := range input.Interventions {
		if !idPattern.MatchString(in.ID) || seen[in.ID] {
			return Set{}, fmt.Errorf("invalid or duplicate intervention id %q", in.ID)
		}
		seen[in.ID] = true
		intervention, err := normalize(in, manifest)
		if err != nil {
			return Set{}, fmt.Errorf("intervention %s: %w", in.ID, err)
		}
		set.interventions = append(set.interventions, intervention)
	}
	return set, nil
}

func normalize(in rawIntervention, manifest compiler.Manifest) (Intervention, error) {
	allowed, ok := fields[in.Kind]
	if !ok {
		return Intervention{}, fmt.Errorf("unsupported kind %q", in.Kind)
	}
	present := map[string]bool{
		"calls": in.Calls != nil, "policy": in.Policy.Kind != 0, "operation": in.Operation != nil,
		"provider": in.Provider != "", "model": in.Model != "", "call": in.Call != "",
		"status": in.Status != nil, "body": in.Body.Kind != 0,
	}
	for _, field := range []string{"calls", "policy", "operation", "provider", "model", "call", "status", "body"} {
		if present[field] && !contains(allowed, field) {
			return Intervention{}, fmt.Errorf("kind %s does not accept %s", in.Kind, field)
		}
	}
	out := Intervention{ID: in.ID, Kind: in.Kind}
	if in.Calls != nil {
		if len(in.Calls) == 0 {
			return Intervention{}, fmt.Errorf("calls must not be empty")
		}
		calls := map[string]bool{}
		for _, call := range in.Calls {
			if calls[call] {
				return Intervention{}, fmt.Errorf("duplicate call %q", call)
			}
			calls[call] = true
		}
		out.Calls = append([]string(nil), in.Calls...)
	}
	switch in.Kind {
	case KindChaosPolicy, KindAuthPolicy:
		if !present["policy"] {
			return Intervention{}, fmt.Errorf("kind %s requires policy", in.Kind)
		}
		if in.Policy.Kind != yaml.MappingNode {
			return Intervention{}, fmt.Errorf("policy must be a mapping")
		}
		raw, err := yaml.Marshal(&in.Policy)
		if err != nil {
			return Intervention{}, err
		}
		var canonical []byte
		if in.Kind == KindChaosPolicy {
			policy, err := chaos.Parse(raw, manifest)
			if err != nil {
				return Intervention{}, err
			}
			canonical, err = policy.CanonicalJSON()
			if err != nil {
				return Intervention{}, err
			}
		} else {
			policy, err := authz.Parse(raw, manifest)
			if err != nil {
				return Intervention{}, err
			}
			canonical, err = policy.CanonicalJSON()
			if err != nil {
				return Intervention{}, err
			}
		}
		out.Policy, out.policyRaw = canonical, raw
	case KindFault:
		if in.Operation != nil && manifest.Operation(*in.Operation) == nil {
			return Intervention{}, fmt.Errorf("unknown fault operation %q", *in.Operation)
		}
		out.Operation = in.Operation
	case KindModel:
		if in.Model == "" {
			return Intervention{}, fmt.Errorf("kind model requires model")
		}
		out.Provider, out.Model = in.Provider, in.Model
	case KindToolResponse:
		if in.Call == "" {
			return Intervention{}, fmt.Errorf("kind tool_response requires call")
		}
		if in.Status == nil {
			return Intervention{}, fmt.Errorf("kind tool_response requires status")
		}
		if *in.Status != 0 && (*in.Status < 100 || *in.Status > 599) {
			return Intervention{}, fmt.Errorf("status %d must be 0 or 100 to 599", *in.Status)
		}
		if !present["body"] {
			return Intervention{}, fmt.Errorf("kind tool_response requires body")
		}
		var value any
		if err := in.Body.Decode(&value); err != nil {
			return Intervention{}, fmt.Errorf("body: %w", err)
		}
		body, err := json.Marshal(value)
		if err != nil {
			return Intervention{}, fmt.Errorf("body: %w", err)
		}
		out.Call, out.Status, out.Body = in.Call, *in.Status, body
	}
	return out, nil
}

func rejectNulls(n *yaml.Node, path string) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" || n.Tag == "!!str" && n.Value == "" {
			return fmt.Errorf("%s must not contain empty or null values", path)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := rejectNulls(n.Content[i+1], path+"."+n.Content[i].Value); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if err := rejectNulls(item, path+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Digest identifies the canonical interventions and the manifest they target.
func (s Set) Digest() string {
	encoded, err := json.Marshal(struct {
		Manifest      string         `json:"manifest"`
		Interventions []Intervention `json:"interventions"`
	}{s.manifest.Digest, s.interventions})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
