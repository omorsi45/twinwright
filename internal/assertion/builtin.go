package assertion

import (
	"context"
	"strings"

	"twinwright/internal/eval"
	"twinwright/internal/store"
)

// Builtins returns the custom evaluators available to every assertion file.
// Callers may add their own before parsing.
func Builtins() Customs {
	return Customs{"scenario_evaluation": scenarioEvaluation}
}

// scenarioEvaluation runs the scenario's built-in Go checks.
func scenarioEvaluation(ctx context.Context, s *store.Store, run store.Run) (bool, string, error) {
	report, err := eval.Evaluate(ctx, s, run.WorldID, run.Scenario)
	if err != nil {
		return false, "", err
	}
	var failed []string
	for _, check := range report.Checks {
		if !check.Passed {
			failed = append(failed, check.Name)
		}
	}
	if len(failed) > 0 {
		return false, "failed checks: " + strings.Join(failed, ", "), nil
	}
	return report.Passed, "", nil
}
