package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"twinwright/internal/agent"
)

// providerConfig is everything a command needs to say to build a provider.
//
// It is one struct rather than a longer argument list because the number of
// things a provider can be told keeps growing, and seven commands build one.
type providerConfig struct {
	Name, Model, Scenario, BaseURL string
	// Timeout is the per-attempt deadline. It must be positive.
	Timeout time.Duration
	// MaxOutputTokens is the output ceiling per model call. Zero means the
	// adapter default: no ceiling sent to an OpenAI-compatible server, and
	// agent.DefaultMaxOutputTokens for Anthropic, whose API requires one.
	MaxOutputTokens int
	// RecordCassette is a file to write every HTTP exchange of the provider to.
	// Only run sets it. Cassette is a file to serve them from instead of the
	// network, and only replay sets it. Neither is registered by providerFlags,
	// and neither is meaningful for a provider that makes no HTTP calls.
	RecordCassette, Cassette string
}

// cassetteKeyPlaceholder stands in for the API key when a cassette is replayed.
// The recorded answers were masked with the real key, so no key is needed or read
// to reproduce them.
const cassetteKeyPlaceholder = "cassette-replay-placeholder"

// providerFlags registers the flags every command that can reach a provider
// shares, and returns the config they fill in once fs has been parsed.
//
// One helper rather than seven copies, so every such command has the same names,
// the same defaults and the same refusals. The provider deadline cannot be
// switched off: an uncancellable call is the failure it replaced, and a run that
// hangs forever reports nothing at all.
//
// The returned config carries only what the flags set. The caller names the
// provider, model and scenario, which it learns from its own flags or from the
// run being resumed; use with to add them.
func providerFlags(fs *flag.FlagSet) *providerConfig {
	cfg := &providerConfig{}
	fs.StringVar(&cfg.BaseURL, "base-url", os.Getenv("OPENAI_BASE_URL"), "base URL for openai-compatible")
	fs.DurationVar(&cfg.Timeout, "provider-timeout", agent.DefaultCallTimeout, "per-attempt provider call deadline")
	fs.IntVar(&cfg.MaxOutputTokens, "max-output-tokens", 0, "output token ceiling per model call; 0 keeps each provider's own default")
	return cfg
}

// recordCassetteFlag registers --record-cassette. It is separate from
// providerFlags because a cassette holds the exchanges of one run: bench, worker
// and fork run many runs or part of one, and resume continues a run whose earlier
// turns were made elsewhere, so only run records.
func recordCassetteFlag(fs *flag.FlagSet, cfg *providerConfig) {
	fs.StringVar(&cfg.RecordCassette, "record-cassette", "", "write the provider's HTTP exchanges to this file (replaced if it exists), for twinwright replay --cassette")
}

// with returns a copy of the flag values aimed at one provider, model and
// scenario. The copy leaves the shared config alone, so a command that builds
// several providers cannot leak one choice into the next.
func (c providerConfig) with(name, model, scenario string) providerConfig {
	c.Name, c.Model, c.Scenario = name, model, scenario
	return c
}

// selectProvider builds the provider for a command that never records. A
// recording owns a file that has to be closed, which this cannot hand back, so a
// config that asks for one is refused; openProvider is the entry point for that.
func selectProvider(cfg providerConfig) (agent.Provider, error) {
	if cfg.RecordCassette != "" {
		return nil, errors.New("--record-cassette needs openProvider, which returns the file to close")
	}
	// Without RecordCassette the closer is a no-op, so dropping it leaks nothing.
	provider, _, err := openProvider(cfg)
	return provider, err
}

// closerFunc adapts a function to io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func noClose() error { return nil }

// openProvider builds the provider for cfg and returns what must be closed when
// the command ends: the cassette being recorded, or nothing. The closer is never
// nil on success, so callers can defer it unconditionally.
//
// With cfg.RecordCassette the adapter is built around a recording transport that
// sits on the default one. With cfg.Cassette the adapter is rebuilt from the
// cassette alone: the provider and model come from the run, the address from the
// first recorded exchange, the output ceiling from the cassette header, the key
// is a placeholder, and the only transport is the cassette's, which never dials.
func openProvider(cfg providerConfig) (agent.Provider, io.Closer, error) {
	if cfg.Timeout <= 0 {
		return nil, nil, fmt.Errorf("provider timeout must be positive")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, nil, fmt.Errorf("--max-output-tokens must not be negative, got %d", cfg.MaxOutputTokens)
	}
	if cfg.Name == "scripted" {
		// A scripted provider makes no HTTP calls, so there is nothing to record
		// and nothing a cassette could stand in for.
		if cfg.RecordCassette != "" {
			return nil, nil, fmt.Errorf("--record-cassette needs a provider that makes HTTP calls; scripted makes none")
		}
		if cfg.Cassette != "" {
			return nil, nil, fmt.Errorf("--cassette needs a run made through an HTTP provider; this run used scripted, which makes no calls")
		}
		fixture := scriptedFixture(cfg.Model, cfg.Scenario)
		if cfg.Model == agent.RegressedModel {
			// The manifest is not known here: the caller that holds it binds it
			// with agent.WithManifest, and the wrapper refuses to run unbound.
			return agent.RegressedProvider{Inner: fixture}, closerFunc(noClose), nil
		}
		return fixture, closerFunc(noClose), nil
	}
	if cfg.Cassette != "" && cfg.RecordCassette != "" {
		return nil, nil, fmt.Errorf("--cassette and --record-cassette cannot be combined")
	}

	var key string
	switch cfg.Name {
	case "openai":
		key = os.Getenv("OPENAI_API_KEY")
		if key == "" && cfg.Cassette == "" {
			return nil, nil, fmt.Errorf("OPENAI_API_KEY is required for openai")
		}
	case "openai-compatible":
		key = os.Getenv("OPENAI_API_KEY")
		if cfg.BaseURL == "" && cfg.Cassette == "" {
			return nil, nil, fmt.Errorf("openai-compatible requires --base-url or OPENAI_BASE_URL")
		}
	case "anthropic":
		key = os.Getenv("ANTHROPIC_API_KEY")
		if key == "" && cfg.Cassette == "" {
			return nil, nil, fmt.Errorf("ANTHROPIC_API_KEY is required for anthropic")
		}
	default:
		return nil, nil, fmt.Errorf("unknown agent %q", cfg.Name)
	}
	if cfg.Model == "" {
		return nil, nil, fmt.Errorf("model is required for %s", cfg.Name)
	}

	var client *http.Client
	closer := closerFunc(noClose)
	// endpoint is the full URL for openai and anthropic, and base the prefix for
	// openai-compatible. Only a cassette sets endpoint: live, an empty one keeps
	// each adapter's own default address.
	var endpoint string
	base, maxTokens := cfg.BaseURL, cfg.MaxOutputTokens
	switch {
	case cfg.Cassette != "":
		cassette, err := agent.LoadCassette(cfg.Cassette)
		if err != nil {
			return nil, nil, err
		}
		if err = cassette.CheckRun(cfg.Name, cfg.Model); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", cfg.Cassette, err)
		}
		if len(cassette.Exchanges) == 0 {
			return nil, nil, fmt.Errorf("cassette %s holds no exchanges, so there is nothing to replay", cfg.Cassette)
		}
		endpoint, maxTokens = cassette.Exchanges[0].URL, cassette.Header.MaxOutputTokens
		if cfg.Name == "openai-compatible" {
			var found bool
			if base, found = strings.CutSuffix(endpoint, "/chat/completions"); !found {
				return nil, nil, fmt.Errorf("cassette %s: the first exchange went to %s, which is not a chat completions endpoint", cfg.Cassette, endpoint)
			}
		}
		key = cassetteKeyPlaceholder
		client = &http.Client{Transport: cassette.Transport()}
	case cfg.RecordCassette != "":
		recording, err := agent.NewRecordingTransportWith(http.DefaultTransport, cfg.RecordCassette,
			agent.CassetteHeader{Provider: cfg.Name, Model: cfg.Model, MaxOutputTokens: cfg.MaxOutputTokens}, key)
		if err != nil {
			return nil, nil, err
		}
		client = &http.Client{Transport: recording}
		closer = recording.Close
	}

	switch cfg.Name {
	case "openai":
		return agent.OpenAIProvider{APIKey: key, Model: cfg.Model, URL: endpoint, Client: client, Timeout: cfg.Timeout, MaxOutputTokens: maxTokens}, closer, nil
	case "openai-compatible":
		return agent.ChatCompletionsProvider{APIKey: key, Model: cfg.Model, BaseURL: base, Client: client, Timeout: cfg.Timeout, MaxOutputTokens: maxTokens}, closer, nil
	default:
		return agent.AnthropicProvider{APIKey: key, Model: cfg.Model, URL: endpoint, Client: client, Timeout: cfg.Timeout, MaxOutputTokens: maxTokens}, closer, nil
	}
}
