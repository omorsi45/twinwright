package counterfactual

import (
	"context"

	"twinwright/internal/assertion"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

// Judge decides whether a completed run meets its success definition. It
// only reads the store.
type Judge struct {
	Name   string
	Digest string
	check  func(ctx context.Context, s *store.Store, run store.Run) (passed bool, failed []string, err error)
}

// AssertionJudge defines success as passing every assertion in the set.
func AssertionJudge(set assertion.Set) Judge {
	return Judge{Name: "assertions", Digest: set.Digest(), check: func(ctx context.Context, s *store.Store, run store.Run) (bool, []string, error) {
		report, err := assertion.Check(ctx, s, run.ID, set)
		if err != nil {
			return false, nil, err
		}
		var failed []string
		for _, result := range report.Results {
			if !result.Passed {
				failed = append(failed, result.ID)
			}
		}
		return report.Passed, failed, nil
	}}
}

// ScenarioJudge defines success as the scenario's built-in evaluation.
func ScenarioJudge() Judge {
	return Judge{Name: "scenario_evaluation", check: func(ctx context.Context, s *store.Store, run store.Run) (bool, []string, error) {
		report, err := eval.Evaluate(ctx, s, run.WorldID, run.Scenario)
		if err != nil {
			return false, nil, err
		}
		var failed []string
		for _, check := range report.Checks {
			if !check.Passed {
				failed = append(failed, check.Name)
			}
		}
		return report.Passed, failed, nil
	}}
}
