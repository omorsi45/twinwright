package container

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config describes an optional sidecar process for a world.
type Config struct {
	Version int
	Name    string
	Runtime string
	Image   string
	Publish []string
	EnvFile string
	Label   string

	// Network is the docker network mode. It is required for the docker runtime
	// rather than defaulted, because a sidecar's reachability is the operator's
	// decision and an implicit default would be a surprising one either way.
	Network string
	// StartupTimeout bounds the wait for a container to become healthy. It is
	// always set: an unbounded wait turns a broken image into a hung CI job
	// instead of a failed one.
	StartupTimeout time.Duration
	// StopTimeout is the grace period a container gets to exit before it is
	// removed.
	StopTimeout time.Duration
	// Health is the readiness probe. Empty means the container is considered
	// ready as soon as docker reports it running, which is the previous
	// behaviour and is honest about proving less.
	Health Health
	// Resources caps what a sidecar may consume.
	Resources Resources
}

// Health is a readiness probe executed inside the container.
type Health struct {
	Command  []string
	Interval time.Duration
	Retries  int
}

// Resources are the limits passed to docker. Each is validated against a strict
// pattern, because these values become command-line arguments.
type Resources struct {
	Memory string
	CPUs   string
	PIDs   int
}

type rawConfig struct {
	Version        int          `yaml:"version"`
	Name           string       `yaml:"name"`
	Runtime        string       `yaml:"runtime"`
	Image          string       `yaml:"image"`
	Publish        []string     `yaml:"publish"`
	EnvFile        string       `yaml:"env_file"`
	Label          string       `yaml:"label"`
	Network        string       `yaml:"network"`
	StartupTimeout string       `yaml:"startup_timeout"`
	StopTimeout    string       `yaml:"stop_timeout"`
	Health         rawHealth    `yaml:"health"`
	Resources      rawResources `yaml:"resources"`
}

type rawHealth struct {
	Command  []string `yaml:"command"`
	Interval string   `yaml:"interval"`
	Retries  *int     `yaml:"retries"`
}

type rawResources struct {
	Memory string `yaml:"memory"`
	CPUs   string `yaml:"cpus"`
	PIDs   *int   `yaml:"pids"`
}

// Bounds. A startup wait longer than ten minutes is a hung job with extra
// steps, and a probe interval under a millisecond is a busy loop.
const (
	DefaultStartupTimeout = 30 * time.Second
	DefaultStopTimeout    = 5 * time.Second
	DefaultHealthInterval = 500 * time.Millisecond
	DefaultHealthRetries  = 20
	maxStartupTimeout     = 10 * time.Minute
	maxStopTimeout        = 5 * time.Minute
	minHealthInterval     = time.Millisecond
	maxHealthRetries      = 10000
	maxHealthArgs         = 32
)

var (
	// A docker network is a predefined mode or a user-defined name. Nothing here
	// may carry whitespace or a shell metacharacter, because it becomes argv.
	networkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	memoryLimit = regexp.MustCompile(`^[0-9]+(b|k|m|g|kb|mb|gb)?$`)
	cpuLimit    = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// Parse validates a version 1 container sidecar config.
func Parse(raw []byte) (Config, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Config{}, fmt.Errorf("container config YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Config{}, fmt.Errorf("container config must be a mapping")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input rawConfig
	if err := decoder.Decode(&input); err != nil {
		return Config{}, fmt.Errorf("container config YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("container config YAML: %w", err)
		}
		return Config{}, fmt.Errorf("container config has multiple YAML documents")
	}
	if input.Version != 1 {
		return Config{}, fmt.Errorf("unsupported container config version %d", input.Version)
	}
	if input.Label != "experimental" {
		return Config{}, fmt.Errorf("container label must be experimental")
	}
	if strings.TrimSpace(input.Name) == "" {
		return Config{}, fmt.Errorf("container name is required")
	}
	switch input.Runtime {
	case "local", "docker":
	default:
		return Config{}, fmt.Errorf("unsupported container runtime %q", input.Runtime)
	}
	if input.Runtime == "docker" {
		if strings.TrimSpace(input.Image) == "" {
			return Config{}, fmt.Errorf("docker runtime requires image")
		}
		if strings.ContainsAny(input.Image, " \t\r\n") {
			return Config{}, fmt.Errorf("docker image must be a single token")
		}
	}
	if input.EnvFile != "" {
		if filepath.IsAbs(input.EnvFile) || strings.HasPrefix(input.EnvFile, "/") || strings.HasPrefix(input.EnvFile, "\\") || filepath.VolumeName(input.EnvFile) != "" {
			return Config{}, fmt.Errorf("env_file must be a relative path")
		}
		cleaned := filepath.ToSlash(filepath.Clean(input.EnvFile))
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return Config{}, fmt.Errorf("env_file escapes the working directory")
		}
		input.EnvFile = cleaned
	}
	for _, p := range input.Publish {
		if strings.TrimSpace(p) == "" || strings.ContainsAny(p, " \t") {
			return Config{}, fmt.Errorf("invalid publish mapping %q", p)
		}
	}

	cfg := Config{
		Version: 1, Name: input.Name, Runtime: input.Runtime, Image: input.Image,
		Publish: append([]string(nil), input.Publish...), EnvFile: input.EnvFile, Label: "experimental",
	}
	var err error
	if cfg.Network, err = parseNetwork(input); err != nil {
		return Config{}, err
	}
	if cfg.StartupTimeout, err = parseDuration("startup_timeout", input.StartupTimeout, DefaultStartupTimeout, maxStartupTimeout); err != nil {
		return Config{}, err
	}
	if cfg.StopTimeout, err = parseDuration("stop_timeout", input.StopTimeout, DefaultStopTimeout, maxStopTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Health, err = parseHealth(input.Health); err != nil {
		return Config{}, err
	}
	if cfg.Resources, err = parseResources(input.Resources); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseNetwork(input rawConfig) (string, error) {
	network := strings.TrimSpace(input.Network)
	if input.Runtime != "docker" {
		if network != "" {
			return "", fmt.Errorf("network applies to the docker runtime only")
		}
		return "", nil
	}
	if network == "" {
		return "", fmt.Errorf("docker runtime: network is required; use none, bridge, host, or a user-defined network name")
	}
	if !networkName.MatchString(network) {
		return "", fmt.Errorf("invalid network %q: use none, bridge, host, or a user-defined network name", input.Network)
	}
	// docker refuses to publish a port on a container with no network stack, so
	// this combination is a config error rather than a runtime surprise.
	if network == "none" && len(input.Publish) > 0 {
		return "", fmt.Errorf("publish requires a network that supports it; network none has no network stack")
	}
	return network, nil
}

func parseDuration(field, value string, fallback, max time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	if parsed > max {
		return 0, fmt.Errorf("%s must not exceed %s", field, max)
	}
	return parsed, nil
}

func parseHealth(input rawHealth) (Health, error) {
	health := Health{}
	if len(input.Command) == 0 {
		if input.Interval != "" || input.Retries != nil {
			return Health{}, fmt.Errorf("health command is required when a health block is present")
		}
		return health, nil
	}
	if len(input.Command) > maxHealthArgs {
		return Health{}, fmt.Errorf("health command must have at most %d arguments", maxHealthArgs)
	}
	for _, arg := range input.Command {
		if strings.TrimSpace(arg) == "" {
			return Health{}, fmt.Errorf("health command arguments must not be empty")
		}
	}
	health.Command = append([]string(nil), input.Command...)
	interval, err := parseDuration("health.interval", input.Interval, DefaultHealthInterval, time.Minute)
	if err != nil {
		return Health{}, err
	}
	if interval < minHealthInterval {
		return Health{}, fmt.Errorf("health.interval must be at least %s", minHealthInterval)
	}
	health.Interval = interval
	health.Retries = DefaultHealthRetries
	if input.Retries != nil {
		if *input.Retries < 1 {
			return Health{}, fmt.Errorf("health retries must be at least 1")
		}
		if *input.Retries > maxHealthRetries {
			return Health{}, fmt.Errorf("health retries must not exceed %d", maxHealthRetries)
		}
		health.Retries = *input.Retries
	}
	return health, nil
}

func parseResources(input rawResources) (Resources, error) {
	resources := Resources{}
	if memory := strings.TrimSpace(input.Memory); memory != "" {
		if !memoryLimit.MatchString(strings.ToLower(memory)) {
			return Resources{}, fmt.Errorf("invalid resources.memory %q: use a byte count with an optional b, k, m or g suffix", input.Memory)
		}
		resources.Memory = memory
	}
	if cpus := strings.TrimSpace(input.CPUs); cpus != "" {
		if !cpuLimit.MatchString(cpus) {
			return Resources{}, fmt.Errorf("invalid resources.cpus %q: use a decimal number of CPUs", input.CPUs)
		}
		resources.CPUs = cpus
	}
	if input.PIDs != nil {
		if *input.PIDs < 1 {
			return Resources{}, fmt.Errorf("resources.pids must be at least 1")
		}
		resources.PIDs = *input.PIDs
	}
	return resources, nil
}
