package improve_test

import (
	"testing"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/improve"
)

func TestSuggest(t *testing.T) {
	t.Parallel()
	j := config.Job{MaxTokens: 8192, MaxSteps: 8, BudgetUSD: 0.1}
	tests := []struct {
		name  string
		st    improve.Stats
		field string
		to    any
	}{
		{"truncated doubles tokens", improve.Stats{Runs: 4, Truncated: 2}, improve.FieldMaxTokens, 16384},
		{"max_steps raised", improve.Stats{Runs: 4, MaxSteps: 1}, improve.FieldMaxSteps, 12},
		{"steps shrink needs history", improve.Stats{Runs: 6, MaxStepsUsed: 3}, improve.FieldMaxSteps, 5},
		{"budget raised", improve.Stats{Runs: 4, Budget: 1, MaxCostUSD: 0.1}, improve.FieldBudgetUSD, 0.15},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got any
			for _, s := range improve.Suggest(j, tc.st) {
				if s.Field == tc.field && s.To != nil {
					got = s.To
				}
			}
			if got != tc.to {
				t.Fatalf("got %v want %v", got, tc.to)
			}
		})
	}
	if s := improve.Suggest(j, improve.Stats{Runs: 3, MaxStepsUsed: 3}); len(s) != 0 {
		t.Fatalf("too few runs should not shrink: %+v", s)
	}
	if s := improve.Suggest(config.Job{MaxTokens: 32768, MaxSteps: 8}, improve.Stats{Runs: 2, Truncated: 2}); len(s) != 1 || s[0].To != nil {
		t.Fatalf("at ceiling only advises: %+v", s)
	}
}
