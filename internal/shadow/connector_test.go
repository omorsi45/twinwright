package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func examplesRoot() string { return filepath.Join("..", "..", "examples") }

// canonical reduces an observation to the pair the comparison actually keys on,
// so a parity check is not distracted by timestamps or actor names.
func canonical(t *testing.T, obs []Observation) []string {
	t.Helper()
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		args, err := json.Marshal(o.Arguments)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, o.OperationID+" "+string(args))
	}
	return out
}

// The boundary only earns its name if two different recorded formats describing
// the same session normalise to the same observations. If they do not, every
// comparison silently depends on which export an operator happened to have.
//
// All three fixtures record one support session: look up C-104, list their
// invoices, refund CH-1002 for 5905. The audit export and the HTTP capture each
// additionally contain one attempt that did not take effect, which must not
// appear as an observed action.
func TestConnectorsAgreeOnTheSameSession(t *testing.T) {
	root := examplesRoot()
	native, err := Load(root, SourceConfig{Type: "file", Path: "shadow/sample-observations.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := Load(root, SourceConfig{Type: "audit_log", Path: "shadow/sample-audit-log.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	http, err := Load(root, SourceConfig{Type: "recorded_http", Path: "shadow/sample-recorded-http.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	want := canonical(t, native)
	if len(want) != 3 {
		t.Fatalf("native fixture should hold 3 observations, got %d", len(want))
	}
	for name, got := range map[string][]Observation{"audit_log": audit, "recorded_http": http} {
		normalised := canonical(t, got)
		if len(normalised) != len(want) {
			t.Fatalf("%s produced %d observations, native produced %d: %v vs %v", name, len(normalised), len(want), normalised, want)
		}
		for i := range want {
			if normalised[i] != want[i] {
				t.Errorf("%s observation %d = %s, native = %s", name, i, normalised[i], want[i])
			}
		}
	}
}

// An attempt that was denied or rejected is evidence that a human tried
// something and was stopped. Counting it as an observed action would make the
// report accuse the agent of missing a step that never happened.
func TestConnectorsDropActionsThatDidNotTakeEffect(t *testing.T) {
	root := examplesRoot()
	audit, err := Load(root, SourceConfig{Type: "audit_log", Path: "shadow/sample-audit-log.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range audit {
		if args, _ := json.Marshal(o.Arguments); strings.Contains(string(args), "99999") {
			t.Fatalf("denied audit entry became an observation: %+v", o)
		}
	}
	http, err := Load(root, SourceConfig{Type: "recorded_http", Path: "shadow/sample-recorded-http.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range http {
		if args, _ := json.Marshal(o.Arguments); strings.Contains(string(args), "CH-9999") {
			t.Fatalf("422 response became an observation: %+v", o)
		}
	}
}

func writeSource(t *testing.T, body string) (string, string) {
	t.Helper()
	root := t.TempDir()
	name := "source.jsonl"
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, name
}

// A misspelled key is the dangerous parse error for a shadow source: lenient
// decoding would drop the field, the observation would load with empty
// arguments, and the report would look clean because the evidence never
// arrived. Strict decoding turns that into a loud failure.
func TestConnectorsRejectUnknownFields(t *testing.T) {
	cases := map[string]struct{ typ, body string }{
		"native": {"file", `{"kind":"human_action","operation_id":"getCustomer","argumnets":{"id":"C-104"}}`},
		"audit":  {"audit_log", `{"timestamp":"t","actor":"a","action":"getCustomer","paramaters":{"id":"C-104"}}`},
		"http":   {"recorded_http", `{"at":"t","method":"GET","path":"/x","operation_id":"getCustomer","bdoy":{"id":"C-104"}}`},
	}
	for name, tc := range cases {
		root, file := writeSource(t, tc.body)
		if _, err := Load(root, SourceConfig{Type: tc.typ, Path: file}); err == nil {
			t.Errorf("%s: misspelled field was accepted", name)
		}
	}
}

// Guessing an operation from method and path would mis-attribute actions, and a
// report naming the wrong operation is worse than one that refuses to load.
func TestRecordedHTTPRequiresAnExplicitOperation(t *testing.T) {
	root, file := writeSource(t, `{"at":"t","method":"POST","path":"/refunds","body":{"charge_id":"CH-1"},"status":201}`)
	_, err := Load(root, SourceConfig{Type: "recorded_http", Path: file})
	if err == nil {
		t.Fatal("a record with no operation_id was accepted")
	}
	if !strings.Contains(err.Error(), "operation_id") {
		t.Fatalf("error should name the missing field, got %v", err)
	}
}

func TestUnknownConnectorIsRejectedAndListsAlternatives(t *testing.T) {
	root, file := writeSource(t, "{}\n")
	_, err := Load(root, SourceConfig{Type: "sftp", Path: file})
	if err == nil {
		t.Fatal("unknown connector was accepted")
	}
	for _, name := range ConnectorNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should list %q as available, got %v", name, err)
		}
	}
}

// Confinement has to hold for every connector, not just the one that shipped
// first, and it has to hold in Load as well as in Parse: Load accepts a
// SourceConfig a caller can build directly, so a guard that only ran during
// config parsing would have a hole in it.
//
// The target outside the root is deliberately VALID native JSONL. An earlier
// version of this test pointed at go.mod and passed even with confinement
// removed, because reading go.mod fails at JSON parsing and the test only
// asserted that some error occurred. A parse failure standing in for a security
// guard is exactly the hole mutation testing is for, so the assertion now
// requires the refusal to name the escape.
func TestEveryConnectorRefusesToEscapeTheExamplesRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "examples")
	if err := os.MkdirAll(filepath.Join(root, "shadow"), 0o750); err != nil {
		t.Fatal(err)
	}
	// Readable, parseable, and outside the root. If confinement fails, this
	// loads cleanly instead of erroring.
	outside := `{"kind":"human_action","operation_id":"getCustomer","arguments":{"id":"C-104"}}` + "\n"
	if err := os.WriteFile(filepath.Join(base, "outside.jsonl"), []byte(outside), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "shadow", "inside.jsonl"), []byte(outside), 0o600); err != nil {
		t.Fatal(err)
	}
	// Control: the same bytes inside the root must load, so a failure above is
	// the guard and not an unrelated read problem.
	if _, err := Load(root, SourceConfig{Type: "file", Path: "shadow/inside.jsonl"}); err != nil {
		t.Fatalf("control load inside the root failed: %v", err)
	}

	escapes := []string{"../outside.jsonl", "shadow/../../outside.jsonl", "../../outside.jsonl"}
	for _, typ := range ConnectorNames() {
		for _, bad := range escapes {
			observations, err := Load(root, SourceConfig{Type: typ, Path: bad})
			if err == nil {
				t.Errorf("%s read %q from outside the root: %+v", typ, bad, observations)
				continue
			}
			if !strings.Contains(err.Error(), "escapes") && !strings.Contains(err.Error(), "must be relative") {
				t.Errorf("%s refused %q for the wrong reason (%v); a parse failure must not stand in for the guard", typ, bad, err)
			}
		}
		abs := filepath.Join(base, "outside.jsonl")
		observations, err := Load(root, SourceConfig{Type: typ, Path: abs})
		if err == nil {
			t.Errorf("%s read an absolute path: %+v", typ, observations)
			continue
		}
		if !strings.Contains(err.Error(), "must be relative") && !strings.Contains(err.Error(), "escapes") {
			t.Errorf("%s refused an absolute path for the wrong reason: %v", typ, err)
		}
	}
}

// A shadow source is someone else's export, so its size is not this project's
// choice. The cap is what turns an oversized dump into an error instead of
// memory pressure.
func TestOversizedSourceIsRefused(t *testing.T) {
	root := t.TempDir()
	name := "huge.jsonl"
	line := `{"kind":"human_action","operation_id":"getCustomer","arguments":{"id":"C-104"}}` + "\n"
	var builder strings.Builder
	for builder.Len() <= maxSourceBytes {
		builder.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(root, SourceConfig{Type: "file", Path: name})
	if err == nil {
		t.Fatal("an oversized source was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error should mention the limit, got %v", err)
	}
}

func TestConnectorNamesAreSortedAndDescribed(t *testing.T) {
	names := ConnectorNames()
	if len(names) < 3 {
		t.Fatalf("expected at least three connectors, got %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("connector names are not sorted: %v", names)
		}
	}
	for _, name := range names {
		c, err := ConnectorFor(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(c.Description()) == "" {
			t.Errorf("connector %q has no description", name)
		}
	}
}

// Every registered connector must be nameable from a config file, or the
// registry and the config validator have drifted apart.
func TestConfigAcceptsEveryRegisteredConnector(t *testing.T) {
	root := examplesRoot()
	for _, name := range ConnectorNames() {
		raw := fmt.Sprintf("version: 1\nmode: observe\nlabel: experimental\nsource:\n  type: %s\n  path: shadow/sample-observations.jsonl\n", name)
		cfg, err := Parse([]byte(raw), root)
		if err != nil {
			t.Errorf("config naming connector %q was rejected: %v", name, err)
			continue
		}
		if cfg.Source.Type != name {
			t.Errorf("parsed source type = %q, want %q", cfg.Source.Type, name)
		}
	}
}
