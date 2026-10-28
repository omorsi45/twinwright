package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

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
	// AgentCommand is the command line of the agent process for the exec
	// provider, split into arguments without a shell by splitAgentCommand.
	// AgentEnv names the environment variables of this process to hand to it.
	// Neither is part of a run: the ledger records the model name that
	// identifies the agent, never how it was started, so both are needed again
	// whenever an exec run is continued.
	AgentCommand string
	AgentEnv     []string
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
	fs.StringVar(&cfg.AgentCommand, "agent-command", "", "command line of the agent process for --agent exec, split on blanks with quotes grouping words and no shell; needed again to resume or serve an exec run")
	fs.Var((*stringList)(&cfg.AgentEnv), "agent-env", "NAME of an environment variable to pass to the exec agent, with this process's value; repeatable. The agent otherwise gets only PATH and HOME")
	return cfg
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
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
	if cfg.Name == "exec" {
		return openExecProvider(cfg)
	}
	// These flags describe an agent process, so with any other provider they
	// would be silently ignored, and an operator who passed them believes an
	// agent of their own is running.
	if cfg.AgentCommand != "" {
		return nil, nil, fmt.Errorf("--agent-command only applies to --agent exec; this run uses %s", cfg.Name)
	}
	if len(cfg.AgentEnv) > 0 {
		return nil, nil, fmt.Errorf("--agent-env only applies to --agent exec; this run uses %s", cfg.Name)
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

// openExecProvider builds the provider that lets a separate program decide.
//
// The provider makes no HTTP calls, so a cassette has nothing to record or
// serve. The model is required because it is the only thing the run keeps to
// say which agent, and which version of it, made the decisions: the command is
// deliberately not stored, and two different programs must not both be recorded
// as an empty model name.
func openExecProvider(cfg providerConfig) (agent.Provider, io.Closer, error) {
	if cfg.RecordCassette != "" {
		return nil, nil, fmt.Errorf("--record-cassette needs a provider that makes HTTP calls; exec makes none")
	}
	if cfg.Cassette != "" {
		return nil, nil, fmt.Errorf("--cassette needs a run made through an HTTP provider; this run used exec, which makes no calls")
	}
	if cfg.Model == "" {
		return nil, nil, fmt.Errorf("exec needs --model to name the agent and its version, for example --model my-agent@1.4")
	}
	if cfg.AgentCommand == "" {
		return nil, nil, fmt.Errorf("exec needs --agent-command to say how to start the agent; the command is not stored with a run, so it is needed again to resume or serve one")
	}
	command, err := splitAgentCommand(cfg.AgentCommand)
	if err != nil {
		return nil, nil, fmt.Errorf("--agent-command: %w", err)
	}
	env, err := resolveAgentEnv(cfg.AgentEnv)
	if err != nil {
		return nil, nil, err
	}
	return agent.ExecProvider{Command: command, Env: env, Timeout: cfg.Timeout}, closerFunc(noClose), nil
}

// splitAgentCommand splits a command line into arguments the way an operator
// reads it, and no further. Words are separated by blanks; a single or double
// quoted span is part of one word, with the quotes removed; text and quoted
// spans that touch make one word. There are no escapes, variables, globs,
// substitutions or operators, because the result is run directly and never
// reaches a shell: what the line says is what is executed.
func splitAgentCommand(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("a %c quote is never closed", quote)
	}
	if inWord {
		words = append(words, word.String())
	}
	if len(words) == 0 {
		return nil, errors.New("the command is empty")
	}
	return words, nil
}

var agentEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// resolveAgentEnv turns --agent-env names into the NAME=value pairs for the
// agent, taking each value from this process's environment.
//
// Only names are accepted. A value typed on the command line is visible in
// process listings and shell history to anyone on the machine, which is what
// passing a secret by environment variable exists to avoid, so NAME=value is
// refused without repeating the value. A name this process does not have is an
// error too: passing nothing would let the agent start without a key it needs
// and fail in a way that looks like the agent's own bug.
func resolveAgentEnv(names []string) ([]string, error) {
	var pairs []string
	seen := map[string]bool{}
	for _, name := range names {
		if before, _, hasValue := strings.Cut(name, "="); hasValue {
			return nil, fmt.Errorf("--agent-env takes the NAME of a variable, not NAME=value, because a value on the command line shows in process listings; export %q instead and pass the name", before)
		}
		if !agentEnvName.MatchString(name) {
			return nil, errors.New("--agent-env needs a variable NAME of letters, digits and underscores that does not start with a digit")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		value, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("--agent-env %s: that variable is not set in this process's environment", name)
		}
		pairs = append(pairs, name+"="+value)
	}
	return pairs, nil
}
