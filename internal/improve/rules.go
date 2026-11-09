package improve

import (
	"fmt"
	"math"
	"strings"

	"github.com/vincent/tarea/internal/config"
)

// Field names a proposal may change. Anything else is rejected.
const (
	FieldPrompt    = "prompt"
	FieldModel     = "model"
	FieldFallbacks = "fallbacks"
	FieldMaxTokens = "max_tokens"
	FieldMaxSteps  = "max_steps"
	FieldBudgetUSD = "budget_usd"
)

const (
	minRunsForShrink = 5
	maxTokensCeiling = 32768
	maxStepsCeiling  = 20
)

// Suggestion is a deterministic tuning hint.
type Suggestion struct {
	Field  string `json:"field"`
	To     any    `json:"to,omitempty"` // nil for an advisory-only hint.
	Reason string `json:"reason"`
}

// Suggest derives numeric tuning from stats; no LLM involved.
func Suggest(j config.Job, st Stats) []Suggestion {
	var out []Suggestion

	if st.Truncated > 0 && j.MaxTokens < maxTokensCeiling {
		to := min(j.MaxTokens*2, maxTokensCeiling)
		out = append(out, Suggestion{FieldMaxTokens, to, fmt.Sprintf(
			"%d/%d runs were cut by the output cap (%d tokens)", st.Truncated, st.Runs, j.MaxTokens)})
	} else if st.Truncated > 0 {
		out = append(out, Suggestion{FieldPrompt, nil, fmt.Sprintf(
			"%d/%d runs truncated even at max_tokens=%d: ask for a shorter answer or split the work", st.Truncated, st.Runs, j.MaxTokens)})
	}

	if st.MaxSteps > 0 && j.MaxSteps < maxStepsCeiling {
		to := min(j.MaxSteps+max(2, j.MaxSteps/2), maxStepsCeiling)
		out = append(out, Suggestion{FieldMaxSteps, to, fmt.Sprintf(
			"%d/%d runs stopped on max_steps=%d", st.MaxSteps, st.Runs, j.MaxSteps)})
	} else if st.Runs >= minRunsForShrink && st.MaxSteps == 0 && st.MaxStepsUsed+2 < j.MaxSteps {
		to := max(st.MaxStepsUsed+2, 3)
		out = append(out, Suggestion{FieldMaxSteps, to, fmt.Sprintf(
			"no run needed more than %d steps (limit %d); a tighter limit caps runaway loops", st.MaxStepsUsed, j.MaxSteps)})
	}

	if st.Budget > 0 {
		to := math.Ceil(st.MaxCostUSD*1.5*100-1e-9) / 100
		if to > j.BudgetUSD {
			out = append(out, Suggestion{FieldBudgetUSD, to, fmt.Sprintf(
				"%d/%d runs stopped on the $%.2f budget (max observed $%.4f)", st.Budget, st.Runs, j.BudgetUSD, st.MaxCostUSD)})
		}
	}

	if st.ToolCutRuns > 0 {
		out = append(out, Suggestion{FieldPrompt, nil, fmt.Sprintf(
			"tool output was cut at the size cap in %d/%d runs: prefer the jq tool or narrower requests", st.ToolCutRuns, st.Runs)})
	}
	if r := st.PromptRatio(); r > 30 {
		out = append(out, Suggestion{FieldPrompt, nil, fmt.Sprintf(
			"prompt tokens are %.0fx completion tokens: context from tool output dominates the cost", r)})
	}
	return out
}

// describe renders suggestions for the meta-prompt.
func describe(ss []Suggestion) string {
	if len(ss) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, s := range ss {
		if s.To != nil {
			fmt.Fprintf(&b, "- %s -> %v: %s\n", s.Field, s.To, s.Reason)
		} else {
			fmt.Fprintf(&b, "- %s: %s\n", s.Field, s.Reason)
		}
	}
	return b.String()
}
