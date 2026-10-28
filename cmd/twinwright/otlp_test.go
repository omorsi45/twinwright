package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// finishedRun drains one scripted run with a worker that does not export, so a
// test can then export its trace with the trace command.
func finishedRun(t *testing.T) (db, runID string) {
	t.Helper()
	dir := t.TempDir()
	manifestPath := buildBillingManifest(t, dir)
	db = filepath.Join(dir, "world.db")
	runID = enqueueBillingRun(t, manifestPath, db)
	if outcomes := drainWorker(t, manifestPath, db); len(outcomes) != 1 {
		t.Fatalf("outcomes=%+v", outcomes)
	}
	return db, runID
}

// The worker scrubs the endpoint from its log line, because it can carry a
// userinfo or a query token. The trace command uses the same exporter and used
// to print the raw URL in its delivery report and in the error for a refusal.
func TestTraceCommandExportDoesNotPrintEndpointCredentials(t *testing.T) {
	const password, token, headerValue = "endpoint-password-not-a-secret", "query-token-not-a-secret", "header-value-not-a-secret"
	db, runID := finishedRun(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	for _, tc := range []struct {
		name   string
		status int
	}{{"delivered", http.StatusOK}, {"refused", http.StatusForbidden}} {
		for _, viaEnv := range []bool{false, true} {
			label := tc.name + "/flag"
			if viaEnv {
				label = tc.name + "/OTEL_EXPORTER_OTLP_ENDPOINT"
			}
			t.Run(label, func(t *testing.T) {
				collector := newTestCollector(t, tc.status)
				endpoint := strings.Replace(collector.URL, "http://", "http://collector:"+password+"@", 1) + "/?token=" + token
				args := []string{"trace", runID, "--format", "otlp", "--db", db, "--otlp-header", "X-Api-Key: " + headerValue}
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				if viaEnv {
					t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
				} else {
					args = append(args, "--otlp-endpoint", endpoint)
				}
				var out bytes.Buffer
				err := runCLI(args, &out)
				text := out.String()
				if err != nil {
					text += "\n" + err.Error()
				}
				if (err == nil) != (tc.status == http.StatusOK) {
					t.Fatalf("status %d gave err=%v", tc.status, err)
				}
				for _, secret := range []string{password, token, headerValue} {
					if strings.Contains(text, secret) {
						t.Errorf("%q reached the output or the error:\n%s", secret, text)
					}
				}
				if tc.status == http.StatusOK {
					var delivery struct {
						Endpoint  string `json:"endpoint"`
						Delivered bool   `json:"delivered"`
					}
					if err := json.Unmarshal(out.Bytes(), &delivery); err != nil || !delivery.Delivered {
						t.Fatalf("delivery: %v\n%s", err, out.String())
					}
					if want := collector.URL + "/v1/traces"; delivery.Endpoint != want {
						t.Errorf("reported endpoint %q, want the address alone, %q", delivery.Endpoint, want)
					}
				}
				// The query reached the collector: it was redacted from the output,
				// not dropped from the request.
				posts := collector.received()
				if len(posts) != 1 || posts[0].Path != "/v1/traces" || posts[0].Query != "token="+token {
					t.Errorf("the collector saw %+v, want one post to /v1/traces with the query kept", posts)
				}
			})
		}
	}
}

// A collector credential on the command line stays in the process listing for
// as long as the worker runs. The OpenTelemetry variable is the standard way to
// pass it without that, so both commands read it when --otlp-header is absent.
func TestResolveOTLPHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env string
		want            map[string]string
		wantErr         string
	}{
		{"neither", "", "", nil, ""},
		{"environment pairs", "", "Authorization=Bearer abc,X-Tenant=acme", map[string]string{"Authorization": "Bearer abc", "X-Tenant": "acme"}, ""},
		{"values are percent-decoded", "", "Authorization=Bearer%20abc%3D%3D,X-Note=a%2Cb", map[string]string{"Authorization": "Bearer abc==", "X-Note": "a,b"}, ""},
		{"a plus is not a space", "", "X-Note=a+b", map[string]string{"X-Note": "a+b"}, ""},
		{"whitespace around names and values is trimmed", "", " X-Tenant = acme , X-Other=1 ", map[string]string{"X-Tenant": "acme", "X-Other": "1"}, ""},
		{"only the first equals sign splits", "", "X-Token=abc==", map[string]string{"X-Token": "abc=="}, ""},
		{"empty entries are skipped", "", ",X-Tenant=acme,,", map[string]string{"X-Tenant": "acme"}, ""},
		{"the flag wins over the environment", "X-Flag: one", "X-Env=two", map[string]string{"X-Flag": "one"}, ""},
		{"a bad flag is not rescued by the environment", "no-colon", "X-Env=two", nil, `--otlp-header must be "Name: value"`},
		{"an entry without equals", "", "X-Tenant=acme,oops", nil, "OTEL_EXPORTER_OTLP_HEADERS entry 2 is not name=value"},
		{"an entry without a name", "", "=value", nil, "OTEL_EXPORTER_OTLP_HEADERS entry 1 is not name=value"},
		{"a name that is not a header name", "", "bad name=value", nil, `OTEL_EXPORTER_OTLP_HEADERS entry 1 has an invalid header name`},
		{"a bad percent escape", "", "X-Secret=ab%zzcdef-not-a-secret", nil, `OTEL_EXPORTER_OTLP_HEADERS entry 1 (X-Secret) has an invalid percent-encoded value`},
		{"a value that decodes to a line break", "", "X-Secret=abc%0d%0aInjected:%20yes", nil, `OTEL_EXPORTER_OTLP_HEADERS entry 1 (X-Secret) has an invalid percent-encoded value`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", tc.env)
			got, err := resolveOTLPHeaders(tc.flag)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err=%v want %q", err, tc.wantErr)
				}
				// The value is a credential, so no refusal repeats it.
				for _, leaked := range []string{"cdef-not-a-secret", "Injected", "abc"} {
					if strings.Contains(err.Error(), leaked) {
						t.Errorf("the error repeats part of a value (%q): %v", leaked, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for key, value := range tc.want {
				if got[key] != value {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

func TestTraceCommandReadsTheOTLPHeaderFromTheEnvironment(t *testing.T) {
	db, runID := finishedRun(t)
	collector := newTestCollector(t, http.StatusOK)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%20env-token-not-a-secret,X-Tenant=acme")
	var out bytes.Buffer
	if err := runCLI([]string{"trace", runID, "--format", "otlp", "--db", db, "--otlp-endpoint", collector.URL}, &out); err != nil {
		t.Fatalf("trace export: %v\n%s", err, out.String())
	}
	posts := collector.received()
	if len(posts) != 1 || posts[0].Header.Get("Authorization") != "Bearer env-token-not-a-secret" || posts[0].Header.Get("X-Tenant") != "acme" {
		t.Fatalf("the collector saw headers %v", posts)
	}
	if strings.Contains(out.String(), "env-token-not-a-secret") {
		t.Errorf("a header value reached the output:\n%s", out.String())
	}

	// --otlp-header still works, and wins when both are given.
	collector = newTestCollector(t, http.StatusOK)
	out.Reset()
	if err := runCLI([]string{"trace", runID, "--format", "otlp", "--db", db, "--otlp-endpoint", collector.URL, "--otlp-header", "X-Tenant: flag"}, &out); err != nil {
		t.Fatalf("trace export: %v\n%s", err, out.String())
	}
	posts = collector.received()
	if len(posts) != 1 || posts[0].Header.Get("X-Tenant") != "flag" || posts[0].Header.Get("Authorization") != "" {
		t.Fatalf("the flag did not take the place of the environment: %v", posts[0].Header)
	}
}

func TestWorkerReadsTheOTLPHeaderFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	manifestPath := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	collector := newTestCollector(t, http.StatusOK)
	enqueueBillingRun(t, manifestPath, db)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%20env-token-not-a-secret,X-Tenant=acme")
	outcomes := drainWorker(t, manifestPath, db, "--otlp-endpoint", collector.URL)
	if len(outcomes) != 1 || !outcomes[0].TraceExported {
		t.Fatalf("outcomes=%+v", outcomes)
	}
	posts := collector.received()
	if len(posts) != 1 || posts[0].Header.Get("Authorization") != "Bearer env-token-not-a-secret" || posts[0].Header.Get("X-Tenant") != "acme" {
		t.Fatalf("the collector saw headers %v", posts)
	}
}

// The variable is only read when a worker is asked to export, and a malformed one
// is refused before the database is touched, like a malformed --otlp-header.
func TestWorkerRejectsAMalformedOTLPHeadersVariableBeforeAnyDatabaseWork(t *testing.T) {
	dir := t.TempDir()
	manifestPath := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "X-Secret=ab%zzcdef-not-a-secret")
	err := runCLI([]string{"worker", "--manifest", manifestPath, "--db", db, "--drain", "--otlp-endpoint", "http://127.0.0.1:1"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_HEADERS entry 1 (X-Secret)") || strings.Contains(err.Error(), "cdef-not-a-secret") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(db); statErr == nil {
		t.Fatal("a rejected header variable still created the database")
	}

	// Without an endpoint the worker does not export, so the variable is not read.
	runID := enqueueBillingRun(t, manifestPath, db)
	if outcomes := drainWorker(t, manifestPath, db); len(outcomes) != 1 || outcomes[0].RunID != runID {
		t.Fatalf("a worker that does not export failed on the variable: %+v", outcomes)
	}
}
