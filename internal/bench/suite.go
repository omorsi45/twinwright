package bench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Suite is a versioned list of benchmark cases.
type Suite struct {
	Version int
	Name    string
	Cases   []Case
}

// Case is one deterministic benchmark scenario configuration.
type Case struct {
	ID         string
	Category   string
	World      string
	Scenario   string
	Chaos      string
	Auth       string
	Assertions string
	Recovery   string
	Fault      string
	Steps      int
	Resume     bool
	ResumeAt   int
	Dimensions []string

	// Expect is the case's declared outcome, ExpectPass (the default) or
	// ExpectFail for a negative control whose fixture or policy is deliberately
	// wrong. It has to be declared here rather than inferred from a case id or a
	// fixture name, because the report can only keep a control out of the
	// compliance rates if the document says which cases are controls.
	Expect string

	// Mode selects how the case executes: local (default) runs it in process,
	// distributed runs it through the worker runtime with an injected crash, and
	// counterfactual runs it locally and then explains its failure.
	Mode string
	// CrashAt is the model turn at which the first worker dies. Distributed mode
	// only.
	CrashAt int
	// Interventions is the intervention set applied by a counterfactual case.
	Interventions string
}

// Execution modes.
const (
	ModeLocal          = "local"
	ModeDistributed    = "distributed"
	ModeCounterfactual = "counterfactual"
)

type rawSuite struct {
	Version int       `yaml:"version"`
	Suite   string    `yaml:"suite"`
	Cases   []rawCase `yaml:"cases"`
}

type rawCase struct {
	ID            string   `yaml:"id"`
	Category      string   `yaml:"category"`
	World         string   `yaml:"world"`
	Scenario      string   `yaml:"scenario"`
	Chaos         string   `yaml:"chaos"`
	Auth          string   `yaml:"auth"`
	Assertions    string   `yaml:"assertions"`
	Recovery      string   `yaml:"recovery"`
	Fault         string   `yaml:"fault"`
	Steps         *int     `yaml:"steps"`
	Resume        *bool    `yaml:"resume"`
	ResumeAt      *int     `yaml:"resume_at"`
	Dimensions    []string `yaml:"dimensions"`
	Expect        string   `yaml:"expect"`
	Mode          string   `yaml:"mode"`
	CrashAt       *int     `yaml:"crash_at"`
	Interventions string   `yaml:"interventions"`
}

var (
	caseID     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	categories = map[string]bool{
		"reliability": true, "reasoning": true, "safety": true,
		"security": true, "recovery": true, "long_horizon": true,
		"distributed": true, "counterfactual": true,
	}
	modes  = map[string]bool{ModeLocal: true, ModeDistributed: true, ModeCounterfactual: true}
	worlds = map[string]bool{"billing": true, "company": true}
	dims   = map[string]bool{
		"task": true, "safety": true, "authorization": true,
		"recovery": true, "duplicate_effects": true, "explanation": true,
	}
)

// Parse validates a suite document. examplesRoot is the repository examples directory used to resolve relative paths.
func Parse(raw []byte, examplesRoot string) (Suite, error) {
	root, err := filepath.Abs(examplesRoot)
	if err != nil {
		return Suite{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return Suite{}, fmt.Errorf("examples root: %w", err)
	}
	if !info.IsDir() {
		return Suite{}, fmt.Errorf("examples root is not a directory")
	}
	var node yaml.Node
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return Suite{}, fmt.Errorf("bench suite YAML: %w", err)
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return Suite{}, fmt.Errorf("bench suite must be a mapping")
	}
	for i := 0; i < len(node.Content[0].Content); i += 2 {
		if node.Content[0].Content[i].Value == "version" && node.Content[0].Content[i+1].Tag != "!!int" {
			return Suite{}, fmt.Errorf("bench suite version must be an integer")
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var input rawSuite
	if err := decoder.Decode(&input); err != nil {
		return Suite{}, fmt.Errorf("bench suite YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Suite{}, fmt.Errorf("bench suite YAML: %w", err)
		}
		return Suite{}, fmt.Errorf("bench suite has multiple YAML documents")
	}
	if input.Version != 1 {
		return Suite{}, fmt.Errorf("unsupported bench suite version %d", input.Version)
	}
	if strings.TrimSpace(input.Suite) == "" {
		return Suite{}, fmt.Errorf("bench suite name is required")
	}
	if len(input.Cases) == 0 {
		return Suite{}, fmt.Errorf("bench suite requires at least one case")
	}
	suite := Suite{Version: 1, Name: input.Suite, Cases: make([]Case, 0, len(input.Cases))}
	ids := map[string]bool{}
	for i, rawCase := range input.Cases {
		c, err := parseCase(rawCase, root, i)
		if err != nil {
			return Suite{}, err
		}
		if ids[c.ID] {
			return Suite{}, fmt.Errorf("duplicate case id %q", c.ID)
		}
		ids[c.ID] = true
		suite.Cases = append(suite.Cases, c)
	}
	return suite, nil
}

func parseCase(input rawCase, root string, index int) (Case, error) {
	if !caseID.MatchString(input.ID) {
		return Case{}, fmt.Errorf("case %d: invalid id %q", index, input.ID)
	}
	if !categories[input.Category] {
		return Case{}, fmt.Errorf("case %q: unknown category %q", input.ID, input.Category)
	}
	if !worlds[input.World] {
		return Case{}, fmt.Errorf("case %q: unknown world %q", input.ID, input.World)
	}
	if strings.TrimSpace(input.Scenario) == "" {
		return Case{}, fmt.Errorf("case %q: scenario is required", input.ID)
	}
	recovery := input.Recovery
	if recovery == "" {
		recovery = "safe"
	}
	if recovery != "safe" && recovery != "unsafe" {
		return Case{}, fmt.Errorf("case %q: recovery must be safe or unsafe", input.ID)
	}
	if recovery != "safe" && input.Scenario != "ambiguous-commit" {
		return Case{}, fmt.Errorf("case %q: recovery unsafe requires ambiguous-commit", input.ID)
	}
	steps := 20
	if input.Steps != nil {
		if *input.Steps < 1 {
			return Case{}, fmt.Errorf("case %q: steps must be positive", input.ID)
		}
		steps = *input.Steps
	}
	resume := false
	if input.Resume != nil {
		resume = *input.Resume
	}
	resumeAt := 0
	if input.ResumeAt != nil {
		if *input.ResumeAt < 1 {
			return Case{}, fmt.Errorf("case %q: resume_at must be positive", input.ID)
		}
		resumeAt = *input.ResumeAt
	}
	if resume && resumeAt == 0 {
		return Case{}, fmt.Errorf("case %q: resume requires resume_at", input.ID)
	}
	if !resume && resumeAt != 0 {
		return Case{}, fmt.Errorf("case %q: resume_at requires resume", input.ID)
	}
	mode := input.Mode
	if mode == "" {
		mode = ModeLocal
	}
	if !modes[mode] {
		return Case{}, fmt.Errorf("case %q: unknown mode %q", input.ID, input.Mode)
	}
	// A field that its mode ignores is refused rather than dropped. A silently
	// ignored crash_at would leave the report claiming a distributed measurement
	// that never took place.
	crashAt := 0
	if input.CrashAt != nil {
		if mode != ModeDistributed {
			return Case{}, fmt.Errorf("case %q: crash_at requires mode distributed", input.ID)
		}
		if *input.CrashAt < 1 {
			return Case{}, fmt.Errorf("case %q: crash_at must be positive", input.ID)
		}
		crashAt = *input.CrashAt
	}
	if mode == ModeDistributed && crashAt == 0 {
		return Case{}, fmt.Errorf("case %q: mode distributed requires crash_at", input.ID)
	}
	if mode == ModeDistributed && resume {
		return Case{}, fmt.Errorf("case %q: resume cannot be combined with mode distributed; a crash and takeover is already a second delivery", input.ID)
	}
	if input.Interventions != "" && mode != ModeCounterfactual {
		return Case{}, fmt.Errorf("case %q: interventions requires mode counterfactual", input.ID)
	}
	if mode == ModeCounterfactual && input.Interventions == "" {
		return Case{}, fmt.Errorf("case %q: mode counterfactual requires interventions", input.ID)
	}
	if len(input.Dimensions) == 0 {
		return Case{}, fmt.Errorf("case %q: at least one dimension is required", input.ID)
	}
	// A misspelled expectation must be refused rather than defaulted to pass: a
	// control silently demoted to an ordinary case would be averaged into the
	// compliance rates as a real failure, and nothing in the report would say so.
	expect := input.Expect
	if expect == "" {
		expect = ExpectPass
	}
	if expect != ExpectPass && expect != ExpectFail {
		return Case{}, fmt.Errorf("case %q: expect must be %q or %q, got %q", input.ID, ExpectPass, ExpectFail, input.Expect)
	}
	seenDim := map[string]bool{}
	dimensions := make([]string, 0, len(input.Dimensions))
	for _, d := range input.Dimensions {
		if !dims[d] {
			return Case{}, fmt.Errorf("case %q: unknown dimension %q", input.ID, d)
		}
		if seenDim[d] {
			return Case{}, fmt.Errorf("case %q: duplicate dimension %q", input.ID, d)
		}
		seenDim[d] = true
		dimensions = append(dimensions, d)
	}
	sort.Strings(dimensions)
	chaos, err := checkPath(root, input.Chaos)
	if err != nil {
		return Case{}, fmt.Errorf("case %q chaos: %w", input.ID, err)
	}
	auth, err := checkPath(root, input.Auth)
	if err != nil {
		return Case{}, fmt.Errorf("case %q auth: %w", input.ID, err)
	}
	assertions, err := checkPath(root, input.Assertions)
	if err != nil {
		return Case{}, fmt.Errorf("case %q assertions: %w", input.ID, err)
	}
	interventions, err := checkPath(root, input.Interventions)
	if err != nil {
		return Case{}, fmt.Errorf("case %q interventions: %w", input.ID, err)
	}
	if input.Fault != "" && chaos != "" {
		return Case{}, fmt.Errorf("case %q: fault and chaos cannot both be set", input.ID)
	}
	return Case{
		ID: input.ID, Category: input.Category, World: input.World, Scenario: input.Scenario,
		Chaos: chaos, Auth: auth, Assertions: assertions, Recovery: recovery, Fault: input.Fault,
		Steps: steps, Resume: resume, ResumeAt: resumeAt, Dimensions: dimensions,
		Expect: expect,
		Mode:   mode, CrashAt: crashAt, Interventions: interventions,
	}, nil
}

// Resolve joins a suite-relative path with the examples root.
func Resolve(examplesRoot, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	return checkPath(examplesRoot, rel)
}

func checkPath(root, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to examples/: %s", rel)
	}
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes examples/: %s", rel)
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
		return "", fmt.Errorf("path escapes examples/: %s", rel)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("missing file %s: %w", rel, err)
	}
	return cleaned, nil
}

// Digest is a stable hash of the suite identity and case ids.
func (s Suite) Digest() string {
	body := map[string]any{"version": s.Version, "suite": s.Name, "cases": s.Cases}
	data, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
