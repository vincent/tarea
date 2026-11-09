// Package improve analyses a job's past runs and proposes a better job
// definition (prompt, numeric limits, model). A human accepts every proposal.
package improve

import (
	"strings"

	"github.com/vincent/tarea/internal/llm"
	"github.com/vincent/tarea/internal/runlog"
)

// toolCutMarker is appended by the agent when a tool result is cut (see agent.truncate).
const toolCutMarker = "...[truncated "

// Stats summarises the analysed runs of one job.
type Stats struct {
	Runs      int `json:"runs"`
	OK        int `json:"ok"`
	Partial   int `json:"partial"`
	Errors    int `json:"errors"`
	Truncated int `json:"truncated"` // runs stopped by finish_reason=length.
	MaxSteps  int `json:"max_steps_stops"`
	Budget    int `json:"budget_stops"`

	AvgSteps      float64 `json:"avg_steps"`
	MaxStepsUsed  int     `json:"max_steps_used"`
	AvgPrompt     float64 `json:"avg_prompt_tokens"`
	AvgCompletion float64 `json:"avg_completion_tokens"`
	AvgCostUSD    float64 `json:"avg_cost_usd"`
	MaxCostUSD    float64 `json:"max_cost_usd"`

	// ToolCutRuns counts runs where at least one tool result was cut at the
	// agent's size cap. Only known for runs whose transcript was loaded.
	ToolCutRuns int `json:"tool_cut_runs"`
	// TruncDetails holds the distinct StopDetail texts of truncated runs.
	TruncDetails []string `json:"trunc_details,omitempty"`
}

// PromptRatio is prompt tokens per completion token (0 when unknown).
func (s Stats) PromptRatio() float64 {
	if s.AvgCompletion == 0 {
		return 0
	}
	return s.AvgPrompt / s.AvgCompletion
}

// Compute aggregates summaries. msgs (optional) returns a run's transcript so
// tool-output cuts can be counted; it is called at most once per run.
func Compute(sums []runlog.Summary, msgs func(runlog.Summary) []llm.Message) Stats {
	var st Stats
	var steps, prompt, completion, cost float64
	seen := map[string]bool{}
	for _, r := range sums {
		st.Runs++
		switch r.Status {
		case runlog.StatusOK:
			st.OK++
		case runlog.StatusPartial:
			st.Partial++
		default:
			st.Errors++
		}
		switch r.Stop {
		case "truncated":
			st.Truncated++
			if d := strings.TrimSpace(r.StopDetail); d != "" && !seen[d] && len(st.TruncDetails) < 3 {
				seen[d] = true
				st.TruncDetails = append(st.TruncDetails, d)
			}
		case "max_steps":
			st.MaxSteps++
		case "budget":
			st.Budget++
		}
		steps += float64(r.Steps)
		prompt += float64(r.PromptTokens)
		completion += float64(r.CompletionTokens)
		cost += r.CostUSD
		st.MaxStepsUsed = max(st.MaxStepsUsed, r.Steps)
		st.MaxCostUSD = max(st.MaxCostUSD, r.CostUSD)
		if msgs != nil && hasToolCut(msgs(r)) {
			st.ToolCutRuns++
		}
	}
	if st.Runs > 0 {
		n := float64(st.Runs)
		st.AvgSteps, st.AvgPrompt, st.AvgCompletion, st.AvgCostUSD = steps/n, prompt/n, completion/n, cost/n
	}
	return st
}

func hasToolCut(ms []llm.Message) bool {
	for _, m := range ms {
		if m.Role == llm.RoleTool && strings.Contains(m.Content, toolCutMarker) {
			return true
		}
	}
	return false
}
