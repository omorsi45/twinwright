package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Connectors adapt a recorded external format into Twinwright observations.
//
// The point of the boundary is that shadow evaluation should not care where an
// observation came from. An organisation already has a record of what its
// humans and systems did - an audit export, a captured HTTP session, a log
// someone sanitised for sharing - and none of those are in Twinwright's native
// shape. Without an adaptor layer the only way to shadow against real history is
// to transform it by hand first, which is where fidelity quietly gets lost.
//
// What every connector has in common is the part that keeps this safe:
//
//   - The transport is a file under the examples root, resolved through
//     confinePath, so a config cannot read an arbitrary path.
//   - Decoding is pure. A connector parses bytes and returns observations. It
//     opens no socket, resolves no host and holds no credential, so a malicious
//     observation file cannot turn shadow mode into a request forwarder.
//   - Nothing is executed. An observation records that an action happened
//     elsewhere. It never becomes a tool call against the local world.
//
// Deliberately NOT implemented here, with reasons rather than silence:
//
//   - A webhook or event-stream transport. That means a listening socket
//     accepting unauthenticated input, which is a materially different security
//     posture from reading a file and needs its own ADR and threat model, not a
//     fourth entry in this registry.
//   - Twinwright's own exported traces as a source. It is an obvious connector
//     to want, but the trace format is owned by internal/trace and pinning a
//     second consumer to it now would freeze a shape that is still moving.
//
// No connector here has been exercised against a live external system. Every
// test in this package runs against fixture bytes. That limitation is stated in
// ADR 0022 and in the README rather than left for a reader to discover.
type Connector interface {
	// Name is the value a shadow config's source.type must carry.
	Name() string
	// Description is one line, shown when a config names an unknown connector.
	Description() string
	// Decode turns recorded bytes into observations, in file order.
	Decode(data []byte) ([]Observation, error)
}

// maxSourceBytes caps an observation source.
//
// A shadow source is someone else's export, so its size is not under this
// project's control. Reading it whole is what every connector here wants, so the
// cap is the thing that stops a 4 GB audit dump from taking the process down
// instead of returning an error.
const maxSourceBytes = 8 << 20 // 8 MiB

var connectors = map[string]Connector{}

func register(c Connector) {
	connectors[c.Name()] = c
}

func init() {
	register(nativeJSONL{name: "file"})
	register(nativeJSONL{name: "jsonl"})
	register(auditLog{})
	register(recordedHTTP{})
}

// ConnectorNames lists registered connectors in sorted order, so an error
// message and a doc table cannot disagree about what is available.
func ConnectorNames() []string {
	names := make([]string, 0, len(connectors))
	for name := range connectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ConnectorFor returns the connector a source type names.
func ConnectorFor(name string) (Connector, error) {
	c, ok := connectors[name]
	if !ok {
		return nil, fmt.Errorf("shadow source type %q is not a known connector; available: %s",
			name, strings.Join(ConnectorNames(), ", "))
	}
	return c, nil
}

// Load reads and decodes an observation source through its connector.
//
// The path is re-confined here even though Parse already validated it, because
// Load accepts a SourceConfig that a caller may have built directly. A guard
// that only runs on the config-file path is a guard with a hole in it.
func Load(examplesRoot string, src SourceConfig) ([]Observation, error) {
	connector, err := ConnectorFor(src.Type)
	if err != nil {
		return nil, err
	}
	cleaned, err := confinePath(examplesRoot, src.Path)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(examplesRoot, filepath.FromSlash(cleaned))
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSourceBytes {
		return nil, fmt.Errorf("shadow source %s is %d bytes, over the %d byte limit", cleaned, info.Size(), maxSourceBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxSourceBytes {
		return nil, fmt.Errorf("shadow source %s is %d bytes, over the %d byte limit", cleaned, len(data), maxSourceBytes)
	}
	observations, err := connector.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s connector: %w", connector.Name(), err)
	}
	return observations, nil
}

// eachJSONLine walks non-empty lines, reporting 1-based line numbers so an error
// points at the offending record in the operator's own file.
func eachJSONLine(data []byte, decode func(line int, raw []byte) error) error {
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if err := decode(i+1, []byte(trimmed)); err != nil {
			return err
		}
	}
	return nil
}

// decodeStrict rejects unknown fields.
//
// Silently ignoring a field is the failure mode that matters for a shadow
// source: a misspelled key would drop real observed behaviour and the report
// would look clean because the evidence never arrived.
func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func validKind(kind string) bool {
	return kind == "human_action" || kind == "external_event"
}

// nativeJSONL reads Twinwright's own observation shape. Registered under both
// "file" and "jsonl": "file" is the name existing configs already use, and
// "jsonl" is what it actually is.
type nativeJSONL struct{ name string }

func (n nativeJSONL) Name() string { return n.name }

func (nativeJSONL) Description() string {
	return "Twinwright's native observation JSONL: kind, at, operation_id, arguments, actor"
}

func (nativeJSONL) Decode(data []byte) ([]Observation, error) {
	var out []Observation
	err := eachJSONLine(data, func(line int, raw []byte) error {
		var obs Observation
		if err := decodeStrict(raw, &obs); err != nil {
			return fmt.Errorf("observation line %d: %w", line, err)
		}
		if !validKind(obs.Kind) {
			return fmt.Errorf("observation line %d: unknown kind %q", line, obs.Kind)
		}
		if obs.OperationID == "" {
			return fmt.Errorf("observation line %d: operation_id required", line)
		}
		if obs.Arguments == nil {
			obs.Arguments = map[string]any{}
		}
		out = append(out, obs)
		return nil
	})
	return out, err
}

// auditRecord is one line of a sanitized audit export.
type auditRecord struct {
	Timestamp  string         `json:"timestamp"`
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	Parameters map[string]any `json:"parameters"`
	Outcome    string         `json:"outcome"`
}

// auditLog reads a sanitized audit export: the shape a compliance or admin log
// usually arrives in, where the operation is an "action" and its inputs are
// "parameters".
type auditLog struct{}

func (auditLog) Name() string { return "audit_log" }

func (auditLog) Description() string {
	return "sanitized audit export JSONL: timestamp, actor, action, parameters, outcome"
}

func (auditLog) Decode(data []byte) ([]Observation, error) {
	var out []Observation
	err := eachJSONLine(data, func(line int, raw []byte) error {
		var record auditRecord
		if err := decodeStrict(raw, &record); err != nil {
			return fmt.Errorf("audit line %d: %w", line, err)
		}
		if record.Action == "" {
			return fmt.Errorf("audit line %d: action required", line)
		}
		// A denied or failed entry is evidence that something was attempted and
		// did not take effect. Treating it as an observed action would make the
		// comparison claim a human did something they were stopped from doing.
		if record.Outcome != "" && record.Outcome != "success" && record.Outcome != "allowed" {
			return nil
		}
		if record.Parameters == nil {
			record.Parameters = map[string]any{}
		}
		out = append(out, Observation{
			Kind:        "human_action",
			At:          record.Timestamp,
			OperationID: record.Action,
			Arguments:   record.Parameters,
			Actor:       record.Actor,
		})
		return nil
	})
	return out, err
}

// httpRecord is one captured request.
//
// OperationID is required rather than inferred from method and path. A recorder
// that captured the call knows which operation it was; guessing here from a
// path pattern would silently mis-attribute an action, and a shadow report that
// names the wrong operation is worse than one that refuses to load.
type httpRecord struct {
	At          string         `json:"at"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	OperationID string         `json:"operation_id"`
	Body        map[string]any `json:"body"`
	Query       map[string]any `json:"query"`
	Actor       string         `json:"actor"`
	Status      int            `json:"status"`
}

// recordedHTTP reads captured HTTP interactions.
type recordedHTTP struct{}

func (recordedHTTP) Name() string { return "recorded_http" }

func (recordedHTTP) Description() string {
	return "recorded HTTP interactions JSONL: at, method, path, operation_id, query, body, status, actor"
}

func (recordedHTTP) Decode(data []byte) ([]Observation, error) {
	var out []Observation
	err := eachJSONLine(data, func(line int, raw []byte) error {
		var record httpRecord
		if err := decodeStrict(raw, &record); err != nil {
			return fmt.Errorf("http line %d: %w", line, err)
		}
		if record.OperationID == "" {
			return fmt.Errorf("http line %d: operation_id required; it is not inferred from method and path", line)
		}
		// 4xx and 5xx mean the call did not take effect, so it is not an
		// observed action. Status 0 means the recorder did not capture one.
		if record.Status >= 400 {
			return nil
		}
		arguments := map[string]any{}
		for k, v := range record.Query {
			arguments[k] = v
		}
		// Body wins a collision: a value sent in the body is the request's
		// actual payload, and a query parameter repeating a body field is
		// routing detail.
		for k, v := range record.Body {
			arguments[k] = v
		}
		out = append(out, Observation{
			Kind:        "external_event",
			At:          record.At,
			OperationID: record.OperationID,
			Arguments:   arguments,
			Actor:       record.Actor,
		})
		return nil
	})
	return out, err
}
