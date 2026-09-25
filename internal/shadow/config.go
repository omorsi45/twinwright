package shadow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is an experimental observe-only shadow configuration.
type Config struct {
	Version   int
	Mode      string
	Label     string
	Source    SourceConfig
	SecretEnv []string
}

// SourceConfig selects a read-only observation source.
type SourceConfig struct {
	Type string
	Path string
}

type rawConfig struct {
	Version   int      `yaml:"version"`
	Mode      string   `yaml:"mode"`
	Label     string   `yaml:"label"`
	SecretEnv []string `yaml:"secret_env"`
	Source    *struct {
		Type string `yaml:"type"`
		Path string `yaml:"path"`
	} `yaml:"source"`
	AllowWrites *bool `yaml:"allow_writes"`
}

// Parse validates an observe-only shadow config. examplesRoot confines relative source paths.
func Parse(raw []byte, examplesRoot string) (Config, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Config{}, fmt.Errorf("shadow config YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Config{}, fmt.Errorf("shadow config must be a mapping")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input rawConfig
	if err := decoder.Decode(&input); err != nil {
		return Config{}, fmt.Errorf("shadow config YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("shadow config YAML: %w", err)
		}
		return Config{}, fmt.Errorf("shadow config has multiple YAML documents")
	}
	if input.Version != 1 {
		return Config{}, fmt.Errorf("unsupported shadow config version %d", input.Version)
	}
	if input.Mode != "observe" {
		return Config{}, fmt.Errorf("shadow mode %q is not allowed; only observe is supported", input.Mode)
	}
	if input.AllowWrites != nil && *input.AllowWrites {
		return Config{}, fmt.Errorf("shadow allow_writes is rejected in this milestone")
	}
	if input.Label != "experimental" {
		return Config{}, fmt.Errorf("shadow label must be experimental")
	}
	if input.Source == nil || input.Source.Type != "file" || strings.TrimSpace(input.Source.Path) == "" {
		return Config{}, fmt.Errorf("shadow source must be a file with a path")
	}
	path, err := confinePath(examplesRoot, input.Source.Path)
	if err != nil {
		return Config{}, err
	}
	for _, name := range input.SecretEnv {
		if strings.TrimSpace(name) == "" || strings.Contains(name, "=") {
			return Config{}, fmt.Errorf("invalid secret_env name %q", name)
		}
	}
	return Config{
		Version: 1, Mode: "observe", Label: "experimental",
		Source: SourceConfig{Type: "file", Path: path}, SecretEnv: append([]string(nil), input.SecretEnv...),
	}, nil
}

func confinePath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("shadow source path must be relative: %s", rel)
	}
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("shadow source path escapes examples/: %s", rel)
	}
	full := filepath.Join(root, filepath.FromSlash(cleaned))
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	relToRoot, err := filepath.Rel(rootAbs, abs)
	if err != nil || strings.HasPrefix(relToRoot, "..") {
		return "", fmt.Errorf("shadow source path escapes examples/: %s", rel)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("shadow source missing: %w", err)
	}
	return cleaned, nil
}

// Digest is a stable hash of the documented config fields.
func (c Config) Digest() string {
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Observation is one recorded external or human action.
type Observation struct {
	Kind        string         `json:"kind"`
	At          string         `json:"at,omitempty"`
	OperationID string         `json:"operation_id"`
	Arguments   map[string]any `json:"arguments"`
	Actor       string         `json:"actor,omitempty"`
}

// LoadObservations reads a JSONL observation file under examplesRoot.
func LoadObservations(examplesRoot string, rel string) ([]Observation, error) {
	path := filepath.Join(examplesRoot, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Observation
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obs Observation
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&obs); err != nil {
			return nil, fmt.Errorf("observation line %d: %w", i+1, err)
		}
		if obs.Kind != "human_action" && obs.Kind != "external_event" {
			return nil, fmt.Errorf("observation line %d: unknown kind %q", i+1, obs.Kind)
		}
		if obs.OperationID == "" {
			return nil, fmt.Errorf("observation line %d: operation_id required", i+1)
		}
		if obs.Arguments == nil {
			obs.Arguments = map[string]any{}
		}
		out = append(out, obs)
	}
	return out, nil
}
