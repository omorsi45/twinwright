package eval

import (
	"context"

	"twinwright/internal/store"
)

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}
type Report struct {
	Passed bool    `json:"passed"`
	Checks []Check `json:"checks"`
}

func DuplicateCharge(ctx context.Context, s *store.Store, worldID string) (Report, error) {
	var expected, duplicateRefunded, legitimateRefunded int64
	if err := s.DB.QueryRowContext(ctx, "SELECT amount_cents,refunded_cents FROM charges WHERE world_id=? AND id='CH-1002'", worldID).Scan(&expected, &duplicateRefunded); err != nil {
		return Report{}, err
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT refunded_cents FROM charges WHERE world_id=? AND id='CH-1001'", worldID).Scan(&legitimateRefunded); err != nil {
		return Report{}, err
	}
	var count, correctCount int
	var sum int64
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*),COALESCE(sum(amount_cents),0) FROM refunds WHERE world_id=?", worldID).Scan(&count, &sum); err != nil {
		return Report{}, err
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=? AND charge_id='CH-1002'", worldID).Scan(&correctCount); err != nil {
		return Report{}, err
	}
	checks := []Check{
		{"correct_charge_identified", correctCount == 1},
		{"legitimate_charge_untouched", legitimateRefunded == 0},
		{"refund_amount_correct", sum == expected && duplicateRefunded == expected},
		{"exactly_one_refund", count == 1},
	}
	passed := true
	for _, check := range checks {
		passed = passed && check.Passed
	}
	return Report{Passed: passed, Checks: checks}, nil
}
