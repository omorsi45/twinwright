package assertion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

// Custom evaluates what the declarative types cannot express. It must only
// read the store.
type Custom func(ctx context.Context, s *store.Store, run store.Run) (passed bool, detail string, err error)

// Customs maps registered custom evaluator names to their code.
type Customs map[string]Custom

// Set is a validated assertion file bound to the manifest it was checked against.
type Set struct {
	Version    int         `json:"version"`
	Assertions []Assertion `json:"assertions"`
	manifest   compiler.Manifest
	customs    Customs
}

// Assertion is the canonical form of one validated assertion.
type Assertion struct {
	ID         string               `json:"id"`
	Type       string               `json:"type"`
	Table      string               `json:"table,omitempty"`
	Where      map[string]Condition `json:"where,omitempty"`
	Count      *Count               `json:"count,omitempty"`
	Entity     *Ref                 `json:"entity,omitempty"`
	Field      string               `json:"field,omitempty"`
	Value      any                  `json:"value,omitempty"`
	ValueFrom  *Ref                 `json:"value_from,omitempty"`
	References string               `json:"references,omitempty"`
	Event      *EventMatch          `json:"event,omitempty"`
	First      *EventMatch          `json:"first,omitempty"`
	Then       *EventMatch          `json:"then,omitempty"`
	Service    string               `json:"service,omitempty"`
	Operation  string               `json:"operation,omitempty"`
	Name       string               `json:"name,omitempty"`
}

// Condition compares a row column by equality or text containment.
type Condition struct {
	Equals   any    `json:"equals,omitempty"`
	Contains string `json:"contains,omitempty"`
}

// Count is one bound: equals, at_least, or at_most.
type Count struct {
	Op string `json:"op"`
	N  int    `json:"n"`
}

// Ref names a row by table and id, and optionally one of its fields.
type Ref struct {
	Table string `json:"table"`
	ID    string `json:"id"`
	Field string `json:"field,omitempty"`
}

// EventMatch selects ledger events by type and top-level payload values.
type EventMatch struct {
	Type  string         `json:"type"`
	Where map[string]any `json:"where,omitempty"`
}

type rawMatch struct {
	Event string         `yaml:"event"`
	Where map[string]any `yaml:"where"`
}

type rawAssertion struct {
	ID         string         `yaml:"id"`
	Type       string         `yaml:"type"`
	Table      string         `yaml:"table"`
	Where      map[string]any `yaml:"where"`
	Equals     *int           `yaml:"equals"`
	AtLeast    *int           `yaml:"at_least"`
	AtMost     *int           `yaml:"at_most"`
	Entity     string         `yaml:"entity"`
	Field      string         `yaml:"field"`
	Value      any            `yaml:"value"`
	ValueFrom  string         `yaml:"value_from"`
	References string         `yaml:"references"`
	Event      string         `yaml:"event"`
	First      *rawMatch      `yaml:"first"`
	Then       *rawMatch      `yaml:"then"`
	Service    string         `yaml:"service"`
	Operation  string         `yaml:"operation"`
	Name       string         `yaml:"name"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Parse validates a version 1 assertion file against a manifest and the
// registered custom evaluators.
func Parse(raw []byte, manifest compiler.Manifest, customs Customs) (Set, error) {
	if err := compiler.ValidateManifest(manifest); err != nil {
		return Set{}, fmt.Errorf("manifest: %w", err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Set{}, fmt.Errorf("assertions YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Set{}, fmt.Errorf("assertions must be a mapping")
	}
	root := node.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "version" && root.Content[i+1].Tag != "!!int" {
			return Set{}, fmt.Errorf("assertions version must be an integer")
		}
	}
	if err := rejectNulls(root, "assertions file"); err != nil {
		return Set{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input struct {
		Version    int            `yaml:"version"`
		Assertions []rawAssertion `yaml:"assertions"`
	}
	if err := decoder.Decode(&input); err != nil {
		return Set{}, fmt.Errorf("assertions YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Set{}, fmt.Errorf("assertions YAML: %w", err)
		}
		return Set{}, fmt.Errorf("assertions file has multiple YAML documents")
	}
	if input.Version != 1 {
		return Set{}, fmt.Errorf("unsupported assertions version %d", input.Version)
	}
	if len(input.Assertions) == 0 {
		return Set{}, fmt.Errorf("assertions file requires at least one assertion")
	}
	services := map[string]bool{}
	for _, op := range manifest.Operations {
		services[strings.SplitN(op.Behavior, ".", 2)[0]] = true
	}
	set := Set{Version: 1, manifest: manifest, customs: customs}
	seen := map[string]bool{}
	for _, in := range input.Assertions {
		if !idPattern.MatchString(in.ID) || seen[in.ID] {
			return Set{}, fmt.Errorf("invalid or duplicate assertion id %q", in.ID)
		}
		seen[in.ID] = true
		a, err := normalize(in, manifest, services, customs)
		if err != nil {
			return Set{}, fmt.Errorf("assertion %s: %w", in.ID, err)
		}
		set.Assertions = append(set.Assertions, a)
	}
	return set, nil
}

// fields lists which raw fields each type may set; anything else is rejected.
var fields = map[string][]string{
	"row_count":          {"table", "where", "bound"},
	"field_equals":       {"entity", "field", "value", "value_from"},
	"relationship":       {"table", "field", "references"},
	"event_count":        {"event", "where", "bound"},
	"event_exists":       {"event", "where"},
	"event_absent":       {"event", "where"},
	"mutation_forbidden": {"service", "operation"},
	"event_order":        {"first", "then"},
	"custom":             {"name"},
}

func normalize(in rawAssertion, manifest compiler.Manifest, services map[string]bool, customs Customs) (Assertion, error) {
	allowed, ok := fields[in.Type]
	if !ok {
		return Assertion{}, fmt.Errorf("unsupported type %q", in.Type)
	}
	present := map[string]bool{
		"table": in.Table != "", "where": in.Where != nil, "entity": in.Entity != "", "field": in.Field != "",
		"value": in.Value != nil, "value_from": in.ValueFrom != "", "references": in.References != "",
		"event": in.Event != "", "first": in.First != nil, "then": in.Then != nil, "service": in.Service != "",
		"operation": in.Operation != "", "name": in.Name != "",
		"bound": in.Equals != nil || in.AtLeast != nil || in.AtMost != nil,
	}
	for field, set := range present {
		if set && !contains(allowed, field) {
			return Assertion{}, fmt.Errorf("type %s does not accept %s", in.Type, field)
		}
	}
	a := Assertion{ID: in.ID, Type: in.Type}
	var err error
	switch in.Type {
	case "row_count":
		if _, ok := tables[in.Table]; !ok {
			return a, fmt.Errorf("unknown table %q", in.Table)
		}
		a.Table = in.Table
		if a.Where, err = rowConditions(in.Table, in.Where); err != nil {
			return a, err
		}
		a.Count, err = bound(in)
	case "field_equals":
		if a.Entity, err = ref(in.Entity, false); err != nil {
			return a, err
		}
		kind, ok := tables[a.Entity.Table][in.Field]
		if !ok {
			return a, fmt.Errorf("unknown field %s.%s", a.Entity.Table, in.Field)
		}
		a.Field = in.Field
		switch {
		case in.Value != nil && in.ValueFrom == "":
			if !matchesKind(in.Value, kind) {
				return a, fmt.Errorf("value for %s must be %s", in.Field, kind)
			}
			a.Value = in.Value
		case in.Value == nil && in.ValueFrom != "":
			if a.ValueFrom, err = ref(in.ValueFrom, true); err != nil {
				return a, err
			}
			if tables[a.ValueFrom.Table][a.ValueFrom.Field] != kind {
				return a, fmt.Errorf("value_from %s has a different type than %s", in.ValueFrom, in.Field)
			}
		default:
			return a, fmt.Errorf("requires exactly one of value or value_from")
		}
	case "relationship":
		columns, ok := tables[in.Table]
		if !ok || columns[in.Field] != text {
			return a, fmt.Errorf("unknown text field %s.%s", in.Table, in.Field)
		}
		if _, ok := tables[in.References]["id"]; !ok {
			return a, fmt.Errorf("references %q must be a table with an id", in.References)
		}
		a.Table, a.Field, a.References = in.Table, in.Field, in.References
	case "event_count", "event_exists", "event_absent":
		if a.Event, err = eventMatch(rawMatch{Event: in.Event, Where: in.Where}); err != nil {
			return a, err
		}
		switch in.Type {
		case "event_count":
			a.Count, err = bound(in)
		case "event_exists":
			a.Type, a.Count = "event_count", &Count{Op: "at_least", N: 1}
		case "event_absent":
			a.Type, a.Count = "event_count", &Count{Op: "equals", N: 0}
		}
	case "mutation_forbidden":
		switch {
		case in.Service != "" && in.Operation == "":
			if !services[in.Service] {
				return a, fmt.Errorf("service %q has no operation in this world", in.Service)
			}
			a.Service = in.Service
		case in.Service == "" && in.Operation != "":
			if manifest.Operation(in.Operation) == nil {
				return a, fmt.Errorf("unknown operation %q", in.Operation)
			}
			a.Operation = in.Operation
		default:
			return a, fmt.Errorf("requires exactly one of service or operation")
		}
	case "event_order":
		if in.First == nil || in.Then == nil {
			return a, fmt.Errorf("requires first and then")
		}
		if a.First, err = eventMatch(*in.First); err != nil {
			return a, err
		}
		a.Then, err = eventMatch(*in.Then)
	case "custom":
		if _, ok := customs[in.Name]; !ok {
			return a, fmt.Errorf("custom evaluator %q is not registered", in.Name)
		}
		a.Name = in.Name
	}
	return a, err
}

func rowConditions(table string, where map[string]any) (map[string]Condition, error) {
	if len(where) == 0 {
		return nil, nil
	}
	out := map[string]Condition{}
	for column, value := range where {
		kind, ok := tables[table][column]
		if !ok {
			return nil, fmt.Errorf("unknown column %s.%s", table, column)
		}
		if operator, ok := value.(map[string]any); ok {
			needle, isString := operator["contains"].(string)
			if len(operator) != 1 || !isString || needle == "" || kind != text {
				return nil, fmt.Errorf("column %s accepts a value or {contains: text} on text columns", column)
			}
			out[column] = Condition{Contains: needle}
			continue
		}
		if !matchesKind(value, kind) {
			return nil, fmt.Errorf("value for %s must be %s", column, kind)
		}
		out[column] = Condition{Equals: value}
	}
	return out, nil
}

func matchesKind(value any, kind string) bool {
	switch value.(type) {
	case string:
		return kind == text
	case int:
		return kind == integer
	}
	return false
}

func bound(in rawAssertion) (*Count, error) {
	var out *Count
	for _, b := range []struct {
		op    string
		value *int
	}{{"equals", in.Equals}, {"at_least", in.AtLeast}, {"at_most", in.AtMost}} {
		if b.value == nil {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("requires exactly one of equals, at_least, or at_most")
		}
		if *b.value < 0 {
			return nil, fmt.Errorf("%s must be nonnegative", b.op)
		}
		out = &Count{Op: b.op, N: *b.value}
	}
	if out == nil {
		return nil, fmt.Errorf("requires one of equals, at_least, or at_most")
	}
	return out, nil
}

// ref parses table.ID, or table.ID.field when withField is set.
func ref(value string, withField bool) (*Ref, error) {
	parts := strings.Split(value, ".")
	want := 2
	if withField {
		want = 3
	}
	if len(parts) != want || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("reference %q must be table.id%s", value, map[bool]string{true: ".field"}[withField])
	}
	columns, ok := tables[parts[0]]
	if !ok || columns["id"] == "" {
		return nil, fmt.Errorf("reference %q needs a table with an id", value)
	}
	r := &Ref{Table: parts[0], ID: parts[1]}
	if withField {
		if _, ok := columns[parts[2]]; !ok {
			return nil, fmt.Errorf("unknown field in reference %q", value)
		}
		r.Field = parts[2]
	}
	return r, nil
}

func eventMatch(in rawMatch) (*EventMatch, error) {
	if !eventTypes[in.Event] {
		return nil, fmt.Errorf("unknown event type %q", in.Event)
	}
	for key, value := range in.Where {
		switch value.(type) {
		case string, int, bool:
		default:
			return nil, fmt.Errorf("event where %s must be a string, integer, or boolean", key)
		}
	}
	match := &EventMatch{Type: in.Event}
	if len(in.Where) > 0 {
		match.Where = in.Where
	}
	return match, nil
}

func rejectNulls(n *yaml.Node, path string) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
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

// Digest identifies the canonical assertion set.
func (s Set) Digest() string {
	encoded, err := json.Marshal(s.Assertions)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
