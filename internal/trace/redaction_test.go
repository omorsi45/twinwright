package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// The model span copies the run's provider error into error.message, and an
// export sends it to a collector. A base URL that does not parse used to put its
// userinfo and query into that error, so the credential left the process with the
// trace. The error is now redacted where the adapter builds it; this holds the
// whole path, from the adapter through the ledger to both trace formats.
func TestBuildNeverCarriesAMalformedBaseURL(t *testing.T) {
	const query, userinfo = "query-key-token", "userinfo-pw-token"
	for name, url := range map[string]string{
		"bad escape":       "http://user:pa%ss-" + userinfo + "@127.0.0.1:1/v1?key=" + query,
		"trailing newline": "http://user:" + userinfo + "@127.0.0.1:1/v1?key=" + query + "\n",
		"leading space":    " http://user:" + userinfo + "@127.0.0.1:1/v1?key=" + query,
		"bad port":         "http://user:" + userinfo + "@127.0.0.1:notaport/v1?key=" + query,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			manifest := billingManifest(t)
			world, err := s.SeedScenario(ctx, 42, manifest.Digest, "duplicate-charge")
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.CreateRunConfigured(ctx, world.ID, "duplicate-charge", "openai-compatible", "local", "task", store.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			runner := agent.Runner{
				Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest,
				Provider: agent.ChatCompletionsProvider{APIKey: "unused-key-not-a-secret", Model: "local", BaseURL: url},
			}
			if _, err = runner.Execute(ctx, run.ID, 3); err == nil {
				t.Fatal("a run against an unusable base URL succeeded")
			}

			built := build(t, s, run.ID)
			encoded, err := json.Marshal(built)
			if err != nil {
				t.Fatal(err)
			}
			otlp, err := OTLP(built)
			if err != nil {
				t.Fatal(err)
			}
			for format, text := range map[string]string{"json": string(encoded), "otlp": string(otlp), "text": Text(built)} {
				for _, secret := range []string{userinfo, query, "pa%ss"} {
					if strings.Contains(text, secret) {
						t.Errorf("the %s trace carries %q:\n%s", format, secret, text)
					}
				}
			}
			if !strings.Contains(string(encoded), "invalid URL") {
				t.Errorf("the model span lost the reason the call failed: %s", encoded)
			}
		})
	}
}
