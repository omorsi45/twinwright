package container

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"

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
}

type rawConfig struct {
	Version int      `yaml:"version"`
	Name    string   `yaml:"name"`
	Runtime string   `yaml:"runtime"`
	Image   string   `yaml:"image"`
	Publish []string `yaml:"publish"`
	EnvFile string   `yaml:"env_file"`
	Label   string   `yaml:"label"`
}

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
	return Config{
		Version: 1, Name: input.Name, Runtime: input.Runtime, Image: input.Image,
		Publish: append([]string(nil), input.Publish...), EnvFile: input.EnvFile, Label: "experimental",
	}, nil
}
