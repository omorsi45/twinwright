package compiler

import (
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "fmt"
 "sort"
 "strings"

 "gopkg.in/yaml.v3"
)

// Manifest is the executable surface. Behavior is selected by an explicit binding.
type Manifest struct {
 Digest string `json:"digest"`
 Operations []Operation `json:"operations"`
}

type Operation struct {
 ID string `json:"id"`
 Method string `json:"method"`
 Path string `json:"path"`
 Behavior string `json:"behavior"`
 Required []string `json:"required"`
 Properties map[string]string `json:"properties"`
}

func (m Manifest) Operation(id string) *Operation {
 for i := range m.Operations { if m.Operations[i].ID == id { return &m.Operations[i] } }
 return nil
}

type apiSpec struct {
 OpenAPI string `yaml:"openapi"`
 Paths map[string]map[string]apiOperation `yaml:"paths"`
}
type apiOperation struct {
 ID string `yaml:"operationId"`
 Parameters []struct {Name string `yaml:"name"`; In string `yaml:"in"`; Required bool `yaml:"required"`; Schema struct {Type string `yaml:"type"`} `yaml:"schema"`} `yaml:"parameters"`
 RequestBody struct {Content map[string]struct {Schema struct {Type string `yaml:"type"`; Required []string `yaml:"required"`; Properties map[string]struct {Type string `yaml:"type"`} `yaml:"properties"`} `yaml:"schema"`} `yaml:"content"`} `yaml:"requestBody"`
}

func Compile(specBytes, bindingBytes []byte) (Manifest, error) {
 var node yaml.Node
 if err := yaml.Unmarshal(specBytes, &node); err != nil { return Manifest{}, fmt.Errorf("OpenAPI YAML: %w", err) }
 if hasRef(&node) { return Manifest{}, fmt.Errorf("external or local reference is unsupported") }
 var spec apiSpec
 if err := node.Decode(&spec); err != nil { return Manifest{}, fmt.Errorf("OpenAPI: %w", err) }
 if !strings.HasPrefix(spec.OpenAPI, "3.1.") || len(spec.Paths) == 0 { return Manifest{}, fmt.Errorf("unsupported OpenAPI version or empty paths") }
 var bindings struct {Operations map[string]string `yaml:"operations"`}
 if err := yaml.Unmarshal(bindingBytes, &bindings); err != nil { return Manifest{}, fmt.Errorf("bindings YAML: %w", err) }
 m := Manifest{}
 seen := map[string]bool{}
 for path, methods := range spec.Paths {
  if !strings.HasPrefix(path, "/") { return Manifest{}, fmt.Errorf("unsupported path %q", path) }
  for method, def := range methods {
   if method != "get" && method != "post" { return Manifest{}, fmt.Errorf("unsupported method %s %s", method, path) }
   if def.ID == "" { return Manifest{}, fmt.Errorf("missing operationId at %s %s", method, path) }
   if seen[def.ID] { return Manifest{}, fmt.Errorf("duplicate operationId %s", def.ID) }; seen[def.ID] = true
   behavior, ok := bindings.Operations[def.ID]
   if !ok || behavior == "" { return Manifest{}, fmt.Errorf("unbound operation %s", def.ID) }
   if behavior != "billing."+def.ID { return Manifest{}, fmt.Errorf("unsupported binding %s for %s", behavior, def.ID) }
   op := Operation{ID:def.ID, Method:strings.ToUpper(method), Path:path, Behavior:behavior, Properties:map[string]string{}}
   for _, p := range def.Parameters {
    if p.In != "path" || !p.Required || p.Schema.Type != "string" { return Manifest{}, fmt.Errorf("unsupported parameter in %s", def.ID) }
    op.Required = append(op.Required, p.Name); op.Properties[p.Name] = "string"
   }
   if method == "post" {
    body, ok := def.RequestBody.Content["application/json"]
    if !ok || body.Schema.Type != "object" { return Manifest{}, fmt.Errorf("unsupported request body in %s", def.ID) }
    for name, p := range body.Schema.Properties {
     if p.Type != "string" && p.Type != "integer" { return Manifest{}, fmt.Errorf("unsupported property %s in %s", name, def.ID) }
     op.Properties[name] = p.Type
    }
    op.Required = append(op.Required, body.Schema.Required...)
   }
   for _, name := range op.Required { if op.Properties[name] == "" { return Manifest{}, fmt.Errorf("required property %s missing schema in %s", name, def.ID) } }
   sort.Strings(op.Required)
   m.Operations = append(m.Operations, op)
  }
 }
 for id := range bindings.Operations { if !seen[id] { return Manifest{}, fmt.Errorf("binding for nonexistent operation %s", id) } }
 sort.Slice(m.Operations, func(i,j int) bool { return m.Operations[i].ID < m.Operations[j].ID })
 raw, err := json.Marshal(m.Operations); if err != nil { return Manifest{}, err }
 digest := sha256.Sum256(raw); m.Digest = hex.EncodeToString(digest[:])
 return m, nil
}

func hasRef(node *yaml.Node) bool {
 if node.Value == "$ref" { return true }
 for _, child := range node.Content { if hasRef(child) { return true } }
 return false
}
