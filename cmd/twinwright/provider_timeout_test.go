package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent"
)

// The deadline is only useful if it reaches the provider that makes the call. A
// provider built without it falls back to the default, which is exactly the
// silent behaviour this replaced.
func TestSelectProviderCarriesThePerAttemptDeadline(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	deadline := 7 * time.Second

	openai, err := selectProvider("openai", "gpt-test", "duplicate-charge", "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := openai.(agent.OpenAIProvider); !ok || p.Timeout != deadline {
		t.Fatalf("openai=%+v", openai)
	}
	anthropic, err := selectProvider("anthropic", "claude-test", "duplicate-charge", "", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := anthropic.(agent.AnthropicProvider); !ok || p.Timeout != deadline {
		t.Fatalf("anthropic=%+v", anthropic)
	}
	compatible, err := selectProvider("openai-compatible", "local", "duplicate-charge", "http://127.0.0.1:1/v1", deadline)
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := compatible.(agent.ChatCompletionsProvider); !ok || p.Timeout != deadline {
		t.Fatalf("openai-compatible=%+v", compatible)
	}
}

// A deadline of zero would be an uncancellable call again, so it is refused
// rather than treated as "no limit".
func TestProviderTimeoutMustBePositive(t *testing.T) {
	if _, err := selectProvider("scripted", "", "duplicate-charge", "", 0); err == nil {
		t.Fatal("a non-positive provider timeout was accepted")
	}
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	if err := runCLI([]string{"build", filepath.Join("..", "..", "examples", "billing", "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	err := runCLI([]string{
		"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest,
		"--db", filepath.Join(dir, "run.db"), "--steps", "1", "--provider-timeout", "0",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "provider timeout") {
		t.Fatalf("the run command did not refuse a zero deadline: %v", err)
	}
}
