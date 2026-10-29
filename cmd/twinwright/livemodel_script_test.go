package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/live-model.sh is the one place a real model makes the decisions, and it
// is run by hand or by an opt-in CI job, so nothing exercised it. These tests run
// it against a local server that answers like an OpenAI-compatible one that has no
// /api/tags (which is every such server but Ollama), and they need bash, jq, curl
// and go. They skip when one is missing and touch no network beyond loopback.

// liveScriptServer answers the duplicate-charge scenario in two calls, a tool call
// and then a final answer, and includes the system_fingerprint Ollama puts in
// every chat completion. It has no /api/tags, like most servers.
func liveScriptServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(string(body), `"role":"tool"`) {
			w.Write([]byte(`{"id":"chatcmpl-1","model":"local-server-1","system_fingerprint":"fp_ollama","choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		w.Write([]byte(`{"id":"chatcmpl-2","model":"local-server-1","system_fingerprint":"fp_ollama","choices":[{"message":{"content":"Done"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// runLiveScript runs the script and returns its combined output, its exit code and
// the directory it wrote to.
func runLiveScript(t *testing.T, baseURL, model, key string) (output string, exit int, dir string) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "curl", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the live-model script needs bash, jq, curl and go", tool)
		}
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "live-model.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	cmd := exec.Command("bash", script, baseURL, model)
	cmd.Dir = filepath.Join("..", "..")
	var env []string
	for _, pair := range os.Environ() {
		if !strings.HasPrefix(pair, "OPENAI_") && !strings.HasPrefix(pair, "TW_LIVE_") {
			env = append(env, pair)
		}
	}
	cmd.Env = append(env, "TW_LIVE_OUT="+dir, "TW_LIVE_PROVIDER_TIMEOUT=30s")
	if key != "" {
		cmd.Env = append(cmd.Env, "OPENAI_API_KEY="+key)
	}
	var combined bytes.Buffer
	cmd.Stdout, cmd.Stderr = &combined, &combined
	if err = cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("the script did not run: %v\n%s", err, combined.String())
		}
		exit = exitErr.ExitCode()
	}
	return combined.String(), exit, dir
}

// A base URL can carry userinfo and a query that names an account, which is why the
// report names only the host and port. The note about a missing model digest
// printed the whole address, on the run of every server without /api/tags.
func TestLiveModelScriptNeverPrintsTheBaseURL(t *testing.T) {
	const password, token = "script-userinfo-not-a-secret", "script-query-not-a-secret"
	server := liveScriptServer(t)
	hostPort := strings.TrimPrefix(server.URL, "http://")
	base := "http://operator:" + password + "@" + hostPort + "/v1?account=" + token

	output, exit, dir := runLiveScript(t, base, "local", "")
	if exit != 0 {
		t.Fatalf("the script failed (exit %d) on a clean run:\n%s", exit, output)
	}
	for _, secret := range []string{password, token, "operator"} {
		if strings.Contains(output, secret) {
			t.Errorf("%q reached the script's output:\n%s", secret, output)
		}
	}
	if !strings.Contains(output, "no model digest from "+hostPort) {
		t.Errorf("the digest note does not name the server by host and port:\n%s", output)
	}
	report, err := os.ReadFile(filepath.Join(dir, "live-model-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(report, []byte(`"base_url_host": "`+hostPort+`"`)) {
		t.Errorf("the report does not name the host and port:\n%s", report)
	}
}

// OPENAI_API_KEY=ollama is the placeholder Ollama's own documentation uses. The
// masks ignore a secret under eight characters on purpose, so that ordinary words
// are never rewritten, and Ollama puts "fp_ollama" in every response. A check
// for such a key finds it in the cassette on a clean run, and fails the script.
func TestLiveModelScriptSkipsTheKeyCheckForAKeyTheMasksIgnore(t *testing.T) {
	server := liveScriptServer(t)
	output, exit, dir := runLiveScript(t, server.URL+"/v1", "local", "ollama")
	if exit != 0 {
		t.Fatalf("a key of six characters failed a clean run (exit %d):\n%s", exit, output)
	}
	if !strings.Contains(output, "shorter than 8 characters") {
		t.Errorf("the script skipped the key check without saying so:\n%s", output)
	}
	cassette, err := os.ReadFile(filepath.Join(dir, "live.cassette.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cassette, []byte("fp_ollama")) {
		t.Fatal("the fixture no longer puts the short key in the cassette, so this test proves nothing")
	}
}

// The ADR says the script fails if the key appears in any file it wrote, and the
// ledger is the file a leak would be most expensive in. A model name is stored in
// the database as typed, so a key that is also part of the model name reaches it,
// and the script must say so for the database itself and not only for the
// step output that carries a copy.
func TestLiveModelScriptChecksTheDatabaseForTheKey(t *testing.T) {
	const key = "live-script-key-12345"
	server := liveScriptServer(t)
	output, exit, _ := runLiveScript(t, server.URL+"/v1", "model-with-"+key, key)
	if exit == 0 {
		t.Fatalf("the script passed with the key in the model name:\n%s", output)
	}
	for _, file := range []string{"live.db", "run.json", "live-model-report.json"} {
		if !strings.Contains(output, "appears in "+file) {
			t.Errorf("the script did not report the key in %s:\n%s", file, output)
		}
	}
}
