package compiler

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorldDefinition describes service inputs and their entity relationships.
type WorldDefinition struct {
	Version       int                `yaml:"version" json:"version"`
	Name          string             `yaml:"name" json:"name"`
	SeedProfile   string             `yaml:"seed_profile" json:"seed_profile"`
	Services      []ServiceSpec      `yaml:"services" json:"services"`
	Entities      []EntitySpec       `yaml:"entities" json:"entities"`
	Relationships []RelationshipSpec `yaml:"relationships" json:"relationships"`
}

type ServiceSpec struct {
	Name     string `yaml:"name" json:"name"`
	Module   string `yaml:"module" json:"module"`
	OpenAPI  string `yaml:"openapi" json:"openapi"`
	Bindings string `yaml:"bindings" json:"bindings"`
}

type EntitySpec struct {
	Service    string   `yaml:"service" json:"service"`
	Name       string   `yaml:"name" json:"name"`
	PrimaryKey string   `yaml:"primary_key" json:"primary_key"`
	Fields     []string `yaml:"fields" json:"fields"`
}

type RelationshipSpec struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

// ParseWorldDefinition validates version 1 metadata and returns canonical set order.
func ParseWorldDefinition(data []byte) (WorldDefinition, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return WorldDefinition{}, fmt.Errorf("world definition YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return WorldDefinition{}, fmt.Errorf("world definition must be a mapping")
	}
	for i := 0; i < len(node.Content[0].Content); i += 2 {
		if node.Content[0].Content[i].Value == "version" && node.Content[0].Content[i+1].Tag != "!!int" {
			return WorldDefinition{}, fmt.Errorf("world version must be an integer")
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var def WorldDefinition
	if err := decoder.Decode(&def); err != nil {
		return WorldDefinition{}, fmt.Errorf("world definition YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return WorldDefinition{}, fmt.Errorf("world definition YAML: %w", err)
		}
		return WorldDefinition{}, fmt.Errorf("world definition has multiple YAML documents")
	}
	if def.Version != 1 {
		return WorldDefinition{}, fmt.Errorf("unsupported world version %d", def.Version)
	}
	if strings.TrimSpace(def.Name) == "" {
		return WorldDefinition{}, fmt.Errorf("world name is required")
	}
	if def.SeedProfile != "company-v1" {
		return WorldDefinition{}, fmt.Errorf("unsupported seed profile %q", def.SeedProfile)
	}
	if len(def.Services) == 0 {
		return WorldDefinition{}, fmt.Errorf("at least one service is required")
	}
	services := make(map[string]bool, len(def.Services))
	for _, service := range def.Services {
		if !validWorldIdentifier(service.Name) {
			return WorldDefinition{}, fmt.Errorf("invalid service name %q", service.Name)
		}
		if services[service.Name] {
			return WorldDefinition{}, fmt.Errorf("duplicate service %q", service.Name)
		}
		services[service.Name] = true
		if !validWorldIdentifier(service.Module) {
			return WorldDefinition{}, fmt.Errorf("invalid module for service %s", service.Name)
		}
		if strings.TrimSpace(service.OpenAPI) == "" {
			return WorldDefinition{}, fmt.Errorf("openapi file is required for service %s", service.Name)
		}
		if strings.TrimSpace(service.Bindings) == "" {
			return WorldDefinition{}, fmt.Errorf("bindings file is required for service %s", service.Name)
		}
	}
	sort.Slice(def.Services, func(i, j int) bool { return def.Services[i].Name < def.Services[j].Name })

	entities := make(map[string]map[string]bool, len(def.Entities))
	for i := range def.Entities {
		entity := &def.Entities[i]
		if !services[entity.Service] {
			return WorldDefinition{}, fmt.Errorf("unknown service %q for entity %s", entity.Service, entity.Name)
		}
		if !validWorldIdentifier(entity.Name) {
			return WorldDefinition{}, fmt.Errorf("invalid entity name %q", entity.Name)
		}
		key := entity.Service + "." + entity.Name
		if _, exists := entities[key]; exists {
			return WorldDefinition{}, fmt.Errorf("duplicate entity %s", key)
		}
		if !validWorldIdentifier(entity.PrimaryKey) {
			return WorldDefinition{}, fmt.Errorf("invalid primary key for entity %s", key)
		}
		fields := make(map[string]bool, len(entity.Fields))
		for _, field := range entity.Fields {
			if !validWorldIdentifier(field) {
				return WorldDefinition{}, fmt.Errorf("invalid field %q for entity %s", field, key)
			}
			if fields[field] {
				return WorldDefinition{}, fmt.Errorf("duplicate field %s in entity %s", field, key)
			}
			fields[field] = true
		}
		if !fields[entity.PrimaryKey] {
			return WorldDefinition{}, fmt.Errorf("primary key %s is not a declared field in entity %s", entity.PrimaryKey, key)
		}
		sort.Strings(entity.Fields)
		entities[key] = fields
	}
	sort.Slice(def.Entities, func(i, j int) bool {
		if def.Entities[i].Service != def.Entities[j].Service {
			return def.Entities[i].Service < def.Entities[j].Service
		}
		return def.Entities[i].Name < def.Entities[j].Name
	})

	relationships := make(map[RelationshipSpec]bool, len(def.Relationships))
	for _, relationship := range def.Relationships {
		if relationships[relationship] {
			return WorldDefinition{}, fmt.Errorf("duplicate relationship %s to %s", relationship.From, relationship.To)
		}
		relationships[relationship] = true
		for _, endpoint := range []string{relationship.From, relationship.To} {
			parts := strings.Split(endpoint, ".")
			if len(parts) != 3 || !validWorldIdentifier(parts[0]) || !validWorldIdentifier(parts[1]) || !validWorldIdentifier(parts[2]) {
				return WorldDefinition{}, fmt.Errorf("invalid relationship endpoint %q", endpoint)
			}
			if !entities[parts[0]+"."+parts[1]][parts[2]] {
				return WorldDefinition{}, fmt.Errorf("unknown relationship endpoint %q", endpoint)
			}
		}
	}
	sort.Slice(def.Relationships, func(i, j int) bool {
		if def.Relationships[i].From != def.Relationships[j].From {
			return def.Relationships[i].From < def.Relationships[j].From
		}
		return def.Relationships[i].To < def.Relationships[j].To
	})
	return def, nil
}

func validWorldIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		letter := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z'
		if !letter && (i == 0 || !(ch >= '0' && ch <= '9' || ch == '_' || ch == '-')) {
			return false
		}
	}
	return true
}
