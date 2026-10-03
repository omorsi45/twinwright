package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// SECURITY.md claims provider errors are recorded without credentials. That is a
// claim about the ledger, and until this test it was only held transitively: the
// provider tests assert on the returned error and RawBody, and the store writes
// whatever it is handed without redacting anything.
//
// The gap mattered because the two ends are far apart. A provider that stopped
// redacting, or a runner that recorded the raw body alongside the redacted error,
// would leave a key in a durable artefact while every existing test stayed green.
//
// The key here matches neither built-in pattern, so what is being held is the
// configured-secret path rather than the regex that would have caught an sk- or
// Bearer value anyway.
func TestProviderErrorReachesTheLedgerWithoutTheKey(t *testing.T) {
	ctx := context.Background()
	const key = "twinwright-fixture-key-abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// The shape a provider actually returns: the key echoed back inside the
		// error it is complaining about.
		w.Write([]byte(`{"error":{"message":"invalid key ` + key + `","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	root := filepath.Join("..", "..", "examples", "billing")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "openai", "gpt-test", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{
		Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest,
		Provider: OpenAIProvider{APIKey: key, Model: "gpt-test", URL: server.URL, Client: server.Client()},
	}
	_, runErr := runner.Execute(ctx, run.ID, 2)
	if runErr == nil {
		t.Fatal("a 401 must fail the run")
	}
	if strings.Contains(runErr.Error(), key) {
		t.Fatalf("the returned error carries the key: %v", runErr)
	}

	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawRedaction bool
	for _, event := range events {
		if strings.Contains(string(event.Payload), key) {
			t.Fatalf("event %d (%s) carries the key: %s", event.Seq, event.Type, event.Payload)
		}
		if strings.Contains(string(event.Payload), "[REDACTED]") {
			sawRedaction = true
		}
	}
	if !sawRedaction {
		t.Fatal("no event carries the redaction marker, so this test is not looking at the error body")
	}
	failed, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Transcript, key) {
		t.Fatalf("the stored transcript carries the key: %s", failed.Transcript)
	}
}
