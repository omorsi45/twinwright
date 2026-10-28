package main

import (
	"flag"
	"fmt"
	"os"
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
}

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

// with returns a copy of the flag values aimed at one provider, model and
// scenario. The copy leaves the shared config alone, so a command that builds
// several providers cannot leak one choice into the next.
func (c providerConfig) with(name, model, scenario string) providerConfig {
	c.Name, c.Model, c.Scenario = name, model, scenario
	return c
}

func selectProvider(cfg providerConfig) (agent.Provider, error) {
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("provider timeout must be positive")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("--max-output-tokens must not be negative, got %d", cfg.MaxOutputTokens)
	}
	switch cfg.Name {
	case "scripted":
		fixture := scriptedFixture(cfg.Model, cfg.Scenario)
		if cfg.Model == agent.RegressedModel {
			// The manifest is not known here: the caller that holds it binds it
			// with agent.WithManifest, and the wrapper refuses to run unbound.
			return agent.RegressedProvider{Inner: fixture}, nil
		}
		return fixture, nil
	case "openai":
		if os.Getenv("OPENAI_API_KEY") == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY is required for openai")
		}
		if cfg.Model == "" {
			return nil, fmt.Errorf("model is required for openai")
		}
		return agent.OpenAIProvider{APIKey: os.Getenv("OPENAI_API_KEY"), Model: cfg.Model, Timeout: cfg.Timeout, MaxOutputTokens: cfg.MaxOutputTokens}, nil
	case "openai-compatible":
		if cfg.BaseURL == "" {
			return nil, fmt.Errorf("openai-compatible requires --base-url or OPENAI_BASE_URL")
		}
		if cfg.Model == "" {
			return nil, fmt.Errorf("model is required for openai-compatible")
		}
		return agent.ChatCompletionsProvider{APIKey: os.Getenv("OPENAI_API_KEY"), Model: cfg.Model, BaseURL: cfg.BaseURL, Timeout: cfg.Timeout, MaxOutputTokens: cfg.MaxOutputTokens}, nil
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required for anthropic")
		}
		if cfg.Model == "" {
			return nil, fmt.Errorf("model is required for anthropic")
		}
		return agent.AnthropicProvider{APIKey: os.Getenv("ANTHROPIC_API_KEY"), Model: cfg.Model, Timeout: cfg.Timeout, MaxOutputTokens: cfg.MaxOutputTokens}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q", cfg.Name)
	}
}
