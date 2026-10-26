package crashmatrix

import (
	"context"
	"fmt"
	"os"

	"twinwright/internal/agent"
	"twinwright/internal/behavior"
	"twinwright/internal/chaos"
	"twinwright/internal/compiler"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

// matrixSteps is the step budget every scenario runs under. It only has to
// exceed the longest scripted run: the runner stops at completion, and a budget
// that cut a run short would pause it instead of exercising recovery.
const matrixSteps = 30

// matrixSeed is the world seed every scenario uses, the same one the benchmark
// suite defaults to, so a matrix failure can be reproduced from a normal run.
const matrixSeed = 42

// Target is everything needed to execute a scenario's agent, independent of
// which store it runs against.
type Target struct {
	Manifest compiler.Manifest
	Provider agent.Provider
	Steps    int
	// MutatingOperation is the write a stale worker attempts. It differs by
	// world because the company world namespaces its operations.
	MutatingOperation string
	// ZombieArguments are valid arguments for MutatingOperation, so a missing
	// fence check would surface as a committed effect rather than a validation
	// error that masks it.
	ZombieArguments map[string]any
}

// Scenario is one scripted run the matrix crashes at every boundary.
type Scenario struct {
	Name   string
	Target Target
	// Seed creates a fresh world and run in s. Each matrix case seeds its own
	// store, so cases cannot influence one another.
	Seed func(ctx context.Context, s *store.Store) (store.Run, error)
	// Judge evaluates the run's persisted world state, the same judgement the
	// benchmark applies to an uncrashed run.
	Judge func(ctx context.Context, s *store.Store, run store.Run) (bool, []string, error)
}

// Scenarios compiles the worlds under examplesRoot and returns the scenarios
// the matrix covers: duplicate-charge, company-incident and ambiguous-commit.
// The last is the one where the response is lost after the write committed, so
// recovery must not repeat it.
func Scenarios(examplesRoot string) ([]Scenario, error) {
	root, err := os.OpenRoot(examplesRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	billing, err := compileBilling(root)
	if err != nil {
		return nil, fmt.Errorf("compile billing: %w", err)
	}
	company, err := compileCompany(root)
	if err != nil {
		return nil, fmt.Errorf("compile company: %w", err)
	}
	policy, err := root.ReadFile("chaos/ambiguous-commit.yaml")
	if err != nil {
		return nil, err
	}
	parsed, err := chaos.Parse(policy, billing)
	if err != nil {
		return nil, fmt.Errorf("parse ambiguous-commit policy: %w", err)
	}
	chaosJSON, err := parsed.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	zombie := func() map[string]any {
		return map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "stale worker retry"}
	}
	return []Scenario{
		newScenario("duplicate-charge", "fixture-v1", duplicateChargeTask, store.RunOptions{}, Target{
			Manifest: billing, Provider: agent.ScriptedProvider{}, Steps: matrixSteps,
			MutatingOperation: "createRefund", ZombieArguments: zombie(),
		}),
		newScenario("company-incident", "fixture-v1", companyIncidentTask, store.RunOptions{}, Target{
			Manifest: company, Provider: agent.CompanyScriptedProvider{Scenario: "company-incident"}, Steps: matrixSteps,
			MutatingOperation: "billing.createRefund", ZombieArguments: zombie(),
		}),
		newScenario("ambiguous-commit", "fixture-safe-v1", ambiguousCommitTask, store.RunOptions{
			ChaosJSON: chaosJSON, ChaosDigest: parsed.Digest(),
		}, Target{
			Manifest: billing, Provider: agent.AmbiguousScriptedProvider{Unsafe: false}, Steps: matrixSteps,
			MutatingOperation: "createRefund", ZombieArguments: zombie(),
		}),
	}, nil
}

// The task strings match internal/bench so a matrix run records the same
// prompt a benchmark run of the scenario does.
const (
	duplicateChargeTask = "Customer C-104 says they were charged twice. Investigate the account and refund only the duplicate charge if appropriate."
	ambiguousCommitTask = "Issue exactly one 500-cent refund on duplicate charge CH-1002. If the write response is lost, verify the charge before deciding whether to retry."
	companyIncidentTask = "Investigate customer C-104 and CRM account A-104. Refund only a duplicate charge. Record the finding in a CRM note. If the account has software incident evidence, create an engineering issue in project PROJ-ENG and notify the support channel in workspace WS-1."
)

func newScenario(name, model, task string, opts store.RunOptions, target Target) Scenario {
	return Scenario{
		Name:   name,
		Target: target,
		Seed: func(ctx context.Context, s *store.Store) (store.Run, error) {
			world, err := s.SeedScenario(ctx, matrixSeed, target.Manifest.Digest, name)
			if err != nil {
				return store.Run{}, err
			}
			return s.CreateRunConfigured(ctx, world.ID, name, "scripted", model, task, opts)
		},
		Judge: func(ctx context.Context, s *store.Store, run store.Run) (bool, []string, error) {
			report, err := eval.Evaluate(ctx, s, run.WorldID, name)
			if err != nil {
				return false, nil, err
			}
			var failed []string
			for _, check := range report.Checks {
				if !check.Passed {
					failed = append(failed, check.Name)
				}
			}
			return report.Passed && len(failed) == 0, failed, nil
		},
	}
}

func compileBilling(root *os.Root) (compiler.Manifest, error) {
	spec, err := root.ReadFile("billing/openapi.yaml")
	if err != nil {
		return compiler.Manifest{}, err
	}
	bindings, err := root.ReadFile("billing/bindings.yaml")
	if err != nil {
		return compiler.Manifest{}, err
	}
	return compiler.Compile(spec, bindings)
}

func compileCompany(root *os.Root) (compiler.Manifest, error) {
	definition, err := root.ReadFile("company/world.yaml")
	if err != nil {
		return compiler.Manifest{}, err
	}
	// Service files are read through a root confined to the world's own
	// directory, so a path in the definition cannot reach outside it.
	company, err := root.OpenRoot("company")
	if err != nil {
		return compiler.Manifest{}, err
	}
	defer company.Close()
	return compiler.CompileWorld(definition, company.ReadFile, behavior.Builtin())
}
