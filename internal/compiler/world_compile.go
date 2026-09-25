package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"twinwright/internal/behavior"
)

type ServiceManifest struct {
	Name       string      `json:"name"`
	Module     string      `json:"module"`
	Digest     string      `json:"digest"`
	Operations []Operation `json:"operations"`
}

type WorldMetadata struct {
	Definition WorldDefinition   `json:"definition"`
	Services   []ServiceManifest `json:"services"`
}

// CompileWorld combines explicitly bound service interfaces into one world.
func CompileWorld(definition []byte, load func(string) ([]byte, error), registry *behavior.Registry) (Manifest, error) {
	if load == nil || registry == nil {
		return Manifest{}, fmt.Errorf("world loader and behavior registry are required")
	}
	def, err := ParseWorldDefinition(definition)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{World: &WorldMetadata{Definition: def}}
	seen := map[string]bool{}
	for _, service := range def.Services {
		spec, err := load(service.OpenAPI)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s OpenAPI: %w", service.Name, err)
		}
		bindings, err := load(service.Bindings)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s bindings: %w", service.Name, err)
		}
		ops, err := compileService(service, spec, bindings, registry)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", service.Name, err)
		}
		for _, op := range ops {
			if seen[op.ID] {
				return Manifest{}, fmt.Errorf("duplicate operation ID %q across services", op.ID)
			}
			seen[op.ID] = true
			m.Operations = append(m.Operations, op)
		}
		sm := ServiceManifest{Name: service.Name, Module: service.Module, Operations: ops}
		sm.Digest, err = digestService(sm)
		if err != nil {
			return Manifest{}, err
		}
		m.World.Services = append(m.World.Services, sm)
	}
	sort.Slice(m.Operations, func(i, j int) bool { return m.Operations[i].ID < m.Operations[j].ID })
	m.Digest, err = digestWorld(m)
	if err != nil {
		return Manifest{}, err
	}
	if err = ValidateManifestWithRegistry(m, registry); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func compileService(service ServiceSpec, specBytes, bindingBytes []byte, registry *behavior.Registry) ([]Operation, error) {
	var node yaml.Node
	if err := yaml.Unmarshal(specBytes, &node); err != nil {
		return nil, fmt.Errorf("OpenAPI YAML: %w", err)
	}
	if hasRef(&node) {
		return nil, fmt.Errorf("$ref is unsupported")
	}
	if err := checkSchemas(&node); err != nil {
		return nil, err
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("OpenAPI root must be a mapping")
	}
	for i := 0; i < len(node.Content[0].Content); i += 2 {
		key := node.Content[0].Content[i].Value
		if key != "openapi" && key != "info" && key != "paths" {
			return nil, fmt.Errorf("unsupported OpenAPI top-level feature %q", key)
		}
	}
	var spec apiSpec
	if err := node.Decode(&spec); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.1.") || len(spec.Paths) == 0 {
		return nil, fmt.Errorf("unsupported OpenAPI version or empty paths")
	}
	var bindings struct {
		Operations map[string]string `yaml:"operations"`
	}
	if err := yaml.Unmarshal(bindingBytes, &bindings); err != nil {
		return nil, fmt.Errorf("bindings YAML: %w", err)
	}
	if len(bindings.Operations) == 0 {
		return nil, fmt.Errorf("service has no behavior bindings")
	}
	var ops []Operation
	seenIDs := map[string]bool{}
	seenRoutes := map[string]bool{}
	for path, methods := range spec.Paths {
		pathArgs, shape, err := worldPath(path)
		if err != nil {
			return nil, err
		}
		for method, def := range methods {
			if method != "get" && method != "post" {
				return nil, fmt.Errorf("unsupported method %s %s", method, path)
			}
			route := method + " " + shape
			if seenRoutes[route] {
				return nil, fmt.Errorf("ambiguous service route %s", route)
			}
			seenRoutes[route] = true
			if def.ID == "" || seenIDs[def.ID] {
				return nil, fmt.Errorf("missing or duplicate operation ID %q", def.ID)
			}
			seenIDs[def.ID] = true
			key, ok := bindings.Operations[def.ID]
			if !ok || !strings.HasPrefix(key, service.Module+".") {
				return nil, fmt.Errorf("missing or wrong-module binding for %s", def.ID)
			}
			if _, ok := registry.Lookup(key); !ok {
				return nil, fmt.Errorf("unregistered behavior %q", key)
			}
			op := Operation{ID: def.ID, Service: service.Name, Method: strings.ToUpper(method), Path: path, Behavior: key, Properties: map[string]string{}}
			if method == "get" {
				if def.RequestBody != nil || len(def.Parameters) != len(pathArgs) {
					return nil, fmt.Errorf("unsupported GET arguments in %s", def.ID)
				}
				for _, parameter := range def.Parameters {
					if parameter.In != "path" || !parameter.Required || parameter.Schema.Type != "string" || !pathArgs[parameter.Name] || op.Properties[parameter.Name] != "" {
						return nil, fmt.Errorf("unsupported path parameter in %s", def.ID)
					}
					op.Properties[parameter.Name] = "string"
					op.Required = append(op.Required, parameter.Name)
				}
			} else {
				if len(pathArgs) != 0 || len(def.Parameters) != 0 || def.RequestBody == nil || !def.RequestBody.Required || len(def.RequestBody.Content) != 1 {
					return nil, fmt.Errorf("unsupported POST arguments in %s", def.ID)
				}
				body, ok := def.RequestBody.Content["application/json"]
				if !ok || body.Schema.Type != "object" || len(body.Schema.Properties) == 0 {
					return nil, fmt.Errorf("unsupported JSON body in %s", def.ID)
				}
				for name, property := range body.Schema.Properties {
					if name == "" || property.Type != "string" && property.Type != "integer" {
						return nil, fmt.Errorf("unsupported property in %s", def.ID)
					}
					op.Properties[name] = property.Type
				}
				op.Required = append(op.Required, body.Schema.Required...)
				if len(op.Required) != len(op.Properties) {
					return nil, fmt.Errorf("optional properties are unsupported in %s", def.ID)
				}
				for _, name := range op.Required {
					if op.Properties[name] == "" {
						return nil, fmt.Errorf("unknown required property %q in %s", name, def.ID)
					}
				}
			}
			sort.Strings(op.Required)
			ops = append(ops, op)
		}
	}
	if len(ops) != len(bindings.Operations) {
		return nil, fmt.Errorf("bindings include nonexistent operations")
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops, nil
}

func worldPath(path string) (map[string]bool, string, error) {
	if !strings.HasPrefix(path, "/") || path == "/" || strings.Contains(path, "?") {
		return nil, "", fmt.Errorf("unsupported path %q", path)
	}
	args := map[string]bool{}
	var shape []string
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" {
			return nil, "", fmt.Errorf("unsupported path %q", path)
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := segment[1 : len(segment)-1]
			if name == "" || args[name] || strings.ContainsAny(name, "{}") {
				return nil, "", fmt.Errorf("unsupported path argument in %q", path)
			}
			args[name] = true
			shape = append(shape, "{}")
		} else {
			if strings.ContainsAny(segment, "{}") {
				return nil, "", fmt.Errorf("unsupported path %q", path)
			}
			shape = append(shape, segment)
		}
	}
	return args, "/" + strings.Join(shape, "/"), nil
}

func digestService(service ServiceManifest) (string, error) {
	value := struct {
		Name       string      `json:"name"`
		Module     string      `json:"module"`
		Operations []Operation `json:"operations"`
	}{service.Name, service.Module, service.Operations}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func digestWorld(m Manifest) (string, error) {
	data, err := json.Marshal(struct {
		World      *WorldMetadata `json:"world"`
		Operations []Operation    `json:"operations"`
	}{m.World, m.Operations})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateManifestWithRegistry validates both the world digest and its runnable bindings.
func ValidateManifestWithRegistry(m Manifest, registry *behavior.Registry) error {
	if m.World == nil {
		return ValidateManifest(m)
	}
	if registry == nil {
		return fmt.Errorf("behavior registry is required")
	}
	definitionBytes, err := yaml.Marshal(m.World.Definition)
	if err != nil {
		return err
	}
	def, err := ParseWorldDefinition(definitionBytes)
	if err != nil || !reflect.DeepEqual(normalizeDefinition(def), normalizeDefinition(m.World.Definition)) {
		return fmt.Errorf("invalid or noncanonical world definition")
	}
	if len(def.Services) != len(m.World.Services) || len(m.Operations) == 0 {
		return fmt.Errorf("world service or operation count differs")
	}
	flat := make([]Operation, 0, len(m.Operations))
	ids := map[string]bool{}
	for i, service := range m.World.Services {
		spec := def.Services[i]
		if service.Name != spec.Name || service.Module != spec.Module || len(service.Operations) == 0 {
			return fmt.Errorf("service metadata differs for %s", spec.Name)
		}
		digest, err := digestService(service)
		if err != nil || digest != service.Digest {
			return fmt.Errorf("service digest mismatch for %s", spec.Name)
		}
		for _, op := range service.Operations {
			if ids[op.ID] || op.Service != service.Name || !strings.HasPrefix(op.Behavior, service.Module+".") {
				return fmt.Errorf("invalid or duplicate operation %s", op.ID)
			}
			ids[op.ID] = true
			if _, ok := registry.Lookup(op.Behavior); !ok {
				return fmt.Errorf("unregistered behavior %q", op.Behavior)
			}
			if err := validateWorldOperation(op); err != nil {
				return err
			}
			flat = append(flat, op)
		}
	}
	sort.Slice(flat, func(i, j int) bool { return flat[i].ID < flat[j].ID })
	if !reflect.DeepEqual(flat, m.Operations) {
		return fmt.Errorf("flattened world operations differ")
	}
	digest, err := digestWorld(m)
	if err != nil || digest != m.Digest {
		return fmt.Errorf("world manifest digest mismatch")
	}
	return nil
}

func validateWorldOperation(op Operation) error {
	pathArgs, _, err := worldPath(op.Path)
	if err != nil || op.ID == "" || len(op.Properties) != len(op.Required) {
		return fmt.Errorf("invalid operation shape for %s", op.ID)
	}
	seen := map[string]bool{}
	for _, name := range op.Required {
		if seen[name] || op.Properties[name] != "string" && op.Properties[name] != "integer" {
			return fmt.Errorf("invalid operation property in %s", op.ID)
		}
		seen[name] = true
	}
	if op.Method == "GET" {
		if len(pathArgs) != len(op.Properties) {
			return fmt.Errorf("invalid GET path parameters in %s", op.ID)
		}
		for name := range op.Properties {
			if !pathArgs[name] || op.Properties[name] != "string" {
				return fmt.Errorf("invalid GET parameter in %s", op.ID)
			}
		}
	} else if op.Method != "POST" || len(pathArgs) != 0 {
		return fmt.Errorf("unsupported method or path in %s", op.ID)
	}
	return nil
}

func normalizeDefinition(def WorldDefinition) WorldDefinition {
	if def.Entities == nil {
		def.Entities = []EntitySpec{}
	}
	if def.Relationships == nil {
		def.Relationships = []RelationshipSpec{}
	}
	return def
}
