package improve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/vincent/tarea/internal/config"
	"github.com/vincent/tarea/internal/fsx"
	"github.com/vincent/tarea/internal/llm"
	"github.com/vincent/tarea/internal/runlog"
)

// ErrNoChanges means the analysis found nothing worth changing.
var ErrNoChanges = errors.New("improve: no changes proposed")

// ErrStale means the job file changed after the proposal was made.
var ErrStale = errors.New("improve: job file changed since the proposal; run improve again")

// ErrNotPending means the proposal was already accepted or rejected.
var ErrNotPending = errors.New("improve: proposal is not pending")

// DefaultWindow is how many recent runs are analysed.
const DefaultWindow = 20

// Runs is the run history the improver reads (runlog.Store satisfies it).
type Runs interface {
	List(job string, limit int) ([]runlog.Summary, error)
	Get(job, id string) (runlog.Record, error)
}

// Chatter is the LLM client (llm.OpenRouter satisfies it).
type Chatter interface {
	Chat(ctx context.Context, req llm.Request) (llm.Response, error)
}

// Improver proposes and applies job improvements.
type Improver struct {
	DataDir  string
	Jobs     config.Dir
	Runs     Runs
	Provider Chatter
	Model    string // analysis model; empty = the job's own model.
	Window   int
	Now      func() time.Time

	mu sync.Mutex
}

// Status of a proposal.
type Status string

// Proposal statuses.
const (
	StatusPending  Status = "pending"
	StatusAccepted Status = "accepted"
	StatusRejected Status = "rejected"
)

// Change is one field edit.
type Change struct {
	Field string `json:"field"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// Proposal is a reviewable change to one job.
type Proposal struct {
	ID          string       `json:"id"`
	Job         string       `json:"job"`
	CreatedAt   time.Time    `json:"created_at"`
	Status      Status       `json:"status"`
	AnalysedBy  string       `json:"analysed_by,omitempty"`
	CostUSD     float64      `json:"cost_usd"`
	Rationale   string       `json:"rationale"`
	Changes     []Change     `json:"changes"`
	Stats       Stats        `json:"stats"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	BaseSHA     string       `json:"base_sha"`
	NewYAML     string       `json:"new_yaml"`
}

func (im *Improver) now() time.Time {
	if im.Now != nil {
		return im.Now()
	}
	return time.Now()
}

// Propose analyses the job's recent runs and stores a pending proposal.
// With nothing to change it returns ErrNoChanges and the (unsaved) proposal
// carrying the rationale.
func (im *Improver) Propose(ctx context.Context, name string) (Proposal, error) {
	job, err := im.Jobs.Get(name)
	if err != nil {
		return Proposal{}, fmt.Errorf("improve: %w", err)
	}
	_, raw, err := im.readJob(name)
	if err != nil {
		return Proposal{}, err
	}
	window := im.Window
	if window <= 0 {
		window = DefaultWindow
	}
	sums, err := im.Runs.List(name, window)
	if err != nil {
		return Proposal{}, fmt.Errorf("improve: list runs: %w", err)
	}
	if len(sums) == 0 {
		return Proposal{}, fmt.Errorf("improve: job %q has no runs yet", name)
	}

	st := Compute(sums, func(s runlog.Summary) []llm.Message {
		rec, gerr := im.Runs.Get(name, s.ID)
		if gerr != nil {
			return nil
		}
		return rec.Messages
	})
	rules := Suggest(job, st)

	p := Proposal{
		ID: runlog.NewID(im.now()), Job: name, CreatedAt: im.now().UTC(), Status: StatusPending,
		Stats: st, Suggestions: rules, BaseSHA: sha(raw),
	}

	edits, err := im.analyse(ctx, &p, job, rules, sums)
	if err != nil {
		return Proposal{}, err
	}

	changes, newYAML, err := im.apply(job, raw, edits, allowedModels(job, sums))
	if err != nil {
		return Proposal{}, err
	}
	if len(changes) == 0 {
		return p, ErrNoChanges
	}
	if _, perr := config.Parse(name, newYAML, im.Jobs.Lookup); perr != nil {
		return Proposal{}, fmt.Errorf("improve: proposed job is invalid: %w", perr)
	}
	p.Changes, p.NewYAML = changes, string(newYAML)
	if err = im.save(p); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// analyse returns the field edits: rule-derived numeric changes as a baseline,
// refined by the LLM. An LLM failure degrades to rules only when they exist.
func (im *Improver) analyse(ctx context.Context, p *Proposal, job config.Job, rules []Suggestion,
	sums []runlog.Summary) (map[string]any, error) {
	edits := map[string]any{}
	for _, s := range rules {
		if s.To != nil {
			edits[s.Field] = s.To
		}
	}
	model := im.Model
	if model == "" {
		model = job.Model
	}
	p.AnalysedBy = model
	llmEdits, rationale, usage, err := im.askLLM(ctx, model, job, p.Stats, rules, sums, allowedModels(job, sums))
	p.CostUSD = usage.CostUSD
	switch {
	case err != nil && len(edits) == 0:
		return nil, err
	case err != nil:
		p.Rationale = fmt.Sprintf("LLM analysis failed (%v); rule-based changes only.", err)
	default:
		p.Rationale = rationale
		maps.Copy(edits, llmEdits)
	}
	return edits, nil
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (im *Improver) jobPath(name string) (string, error) {
	if !fsx.ValidName(name) {
		return "", fmt.Errorf("%w: %q", config.ErrJobNotFound, name)
	}
	for _, ext := range []string{".yaml", ".yml"} {
		p := filepath.Join(im.Jobs.Path, name+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %q", config.ErrJobNotFound, name)
}

func (im *Improver) readJob(name string) (path string, raw []byte, err error) {
	path, err = im.jobPath(name)
	if err != nil {
		return "", nil, err
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("improve: read %s: %w", path, err)
	}
	return path, raw, nil
}

func allowedModels(j config.Job, sums []runlog.Summary) []string {
	set := []string{j.Model}
	set = append(set, j.Fallbacks...)
	for _, s := range sums {
		if s.Model != "" && !slices.Contains(set, s.Model) {
			set = append(set, s.Model)
		}
	}
	return slices.Compact(set)
}

// --- LLM stage --------------------------------------------------------------

const systemPrompt = `You tune scheduled LLM jobs. You get a job definition, statistics from its recent runs and one successful run transcript. Propose the smallest set of edits that lowers cost/tokens and avoids truncated or partial runs without changing what the job does.

Rules:
- You may only change: prompt, model, fallbacks, max_tokens, max_steps, budget_usd.
- model and fallbacks may only use models from the "allowed models" list.
- Keep the prompt's intent, output format and language. Tighten it: remove redundancy, tell the job to use the jq tool or narrow requests when tool output is large, cap answer length when truncation happens.
- If the job is fine, return no changes.
Reply with ONLY a JSON object: {"rationale": "<short, plain>", "changes": {"<field>": <new value>, ...}}.`

func (im *Improver) askLLM(ctx context.Context, model string, j config.Job, st Stats, rules []Suggestion,
	sums []runlog.Summary, allowed []string) (edits map[string]any, rationale string, usage llm.Usage, err error) {
	if im.Provider == nil {
		return nil, "", llm.Usage{}, errors.New("improve: no LLM provider configured")
	}
	statsJSON, _ := json.MarshalIndent(st, "", "  ")
	tools := make([]string, 0, len(j.MCP))
	for _, m := range j.MCP {
		target := m.Name
		if m.Builtin != "" {
			target += " (builtin " + m.Builtin + ")"
		}
		tools = append(tools, fmt.Sprintf("%s allow=%v", target, m.Allow))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Job %q\nmodel: %s\nfallbacks: %v\nmax_tokens: %d\nmax_steps: %d\nbudget_usd: %v\ntools: %s\n\n### prompt\n%s\n\n",
		j.Name, j.Model, j.Fallbacks, j.MaxTokens, j.MaxSteps, j.BudgetUSD, strings.Join(tools, "; "), j.Prompt)
	fmt.Fprintf(&b, "## Allowed models\n%s\n\n## Stats (last %d runs)\n%s\n\n## Rule-based hints\n%s\n", strings.Join(allowed, ", "), st.Runs, statsJSON, describe(rules))
	if ex := im.exemplar(j.Name, sums); ex != "" {
		fmt.Fprintf(&b, "\n## Last successful run\n%s\n", ex)
	}

	resp, err := im.Provider.Chat(ctx, llm.Request{
		Model:     model,
		MaxTokens: 4096,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: b.String()},
		},
	})
	if err != nil {
		return nil, "", resp.Usage, fmt.Errorf("improve: llm: %w", err)
	}
	edits, rationale, err = parseReply(resp.Message.Content)
	return edits, rationale, resp.Usage, err
}

// exemplar renders the newest successful run compactly: tool calls and trimmed outputs.
func (im *Improver) exemplar(job string, sums []runlog.Summary) string {
	for _, s := range sums {
		if s.Status != runlog.StatusOK {
			continue
		}
		rec, err := im.Runs.Get(job, s.ID)
		if err != nil {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "steps=%d tools=%d tokens=%d/%d cost=$%.4f\n", s.Steps, s.ToolCalls, s.PromptTokens, s.CompletionTokens, s.CostUSD)
		for _, m := range rec.Messages {
			switch m.Role {
			case llm.RoleAssistant:
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "call %s %s\n", tc.Name, clip(tc.Arguments, 200))
				}
			case llm.RoleSystem, llm.RoleUser:
				// not part of the exemplar.
			case llm.RoleTool:
				fmt.Fprintf(&b, "  -> %d bytes: %s\n", len(m.Content), clip(m.Content, 200))
			}
		}
		fmt.Fprintf(&b, "final answer: %s\n", clip(rec.Output, 1500))
		return b.String()
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func parseReply(text string) (changes map[string]any, rationale string, err error) {
	text = strings.TrimSpace(text)
	if i, k := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && k > i {
		text = text[i : k+1]
	}
	var out struct {
		Rationale string         `json:"rationale"`
		Changes   map[string]any `json:"changes"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, "", fmt.Errorf("improve: unreadable LLM reply: %w", err)
	}
	return out.Changes, out.Rationale, nil
}

// --- applying edits to the raw YAML ----------------------------------------

// apply validates edits against the whitelist and bounds, then patches the raw
// YAML so comments, ${VAR} placeholders and untouched fields survive verbatim.
func (im *Improver) apply(j config.Job, raw []byte, edits map[string]any, allowed []string) (changes []Change, out []byte, err error) {
	var doc yaml.Node
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("improve: parse yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("improve: job file is not a YAML mapping")
	}
	root := doc.Content[0]

	fields := make([]string, 0, len(edits))
	for f := range edits {
		fields = append(fields, f)
	}
	slices.Sort(fields)

	for _, f := range fields {
		from, to, node, nerr := normalise(j, f, edits[f], allowed)
		if nerr != nil {
			continue // a bad suggestion is dropped, never applied.
		}
		if reflect.DeepEqual(from, to) {
			continue
		}
		setKey(root, f, node)
		changes = append(changes, Change{Field: f, From: from, To: to})
	}
	if len(changes) == 0 {
		return nil, raw, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(&doc); err != nil {
		return nil, nil, fmt.Errorf("improve: encode yaml: %w", err)
	}
	_ = enc.Close()
	return changes, buf.Bytes(), nil
}

// normaliser validates one field's new value and returns the old value, the
// new value and the YAML node to write.
type normaliser func(j config.Job, v any, allowed []string) (from, to any, node *yaml.Node, err error)

var normalisers = map[string]normaliser{
	FieldPrompt:    normPrompt,
	FieldModel:     normModel,
	FieldFallbacks: normFallbacks,
	FieldMaxTokens: normInt(FieldMaxTokens, 256, 65536, func(j config.Job) int { return j.MaxTokens }),
	FieldMaxSteps:  normInt(FieldMaxSteps, 1, 30, func(j config.Job) int { return j.MaxSteps }),
	FieldBudgetUSD: normBudget,
}

func normalise(j config.Job, field string, v any, allowed []string) (from, to any, node *yaml.Node, err error) {
	n, ok := normalisers[field]
	if !ok {
		return nil, nil, nil, fmt.Errorf("improve: %s: not an editable field", field)
	}
	return n(j, v, allowed)
}

func normPrompt(j config.Job, v any, _ []string) (from, to any, node *yaml.Node, err error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return nil, nil, nil, errors.New("improve: prompt: must be a non-empty string")
	}
	text := strings.TrimRight(s, "\n") + "\n"
	n := scalar("!!str", text)
	if strings.Contains(s, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return j.Prompt, text, n, nil
}

func normModel(j config.Job, v any, allowed []string) (from, to any, node *yaml.Node, err error) {
	s, ok := v.(string)
	if !ok || !slices.Contains(allowed, s) {
		return nil, nil, nil, errors.New("improve: model: not in the allowed list")
	}
	return j.Model, s, scalar("!!str", s), nil
}

func normFallbacks(j config.Job, v any, allowed []string) (from, to any, node *yaml.Node, err error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, nil, nil, errors.New("improve: fallbacks: must be a list")
	}
	list := make([]string, 0, len(raw))
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, e := range raw {
		s, isStr := e.(string)
		if !isStr || !slices.Contains(allowed, s) {
			return nil, nil, nil, errors.New("improve: fallbacks: model not in the allowed list")
		}
		list = append(list, s)
		seq.Content = append(seq.Content, scalar("!!str", s))
	}
	return j.Fallbacks, list, seq, nil
}

func normInt(field string, lo, hi int, cur func(config.Job) int) normaliser {
	return func(j config.Job, v any, _ []string) (from, to any, node *yaml.Node, err error) {
		n, ok := toInt(v)
		if !ok || n < lo || n > hi {
			return nil, nil, nil, fmt.Errorf("improve: %s: out of range %d..%d", field, lo, hi)
		}
		return cur(j), n, scalar("!!int", strconv.Itoa(n)), nil
	}
}

func normBudget(j config.Job, v any, _ []string) (from, to any, node *yaml.Node, err error) {
	f, ok := v.(float64)
	if i, isInt := toInt(v); isInt {
		f, ok = float64(i), true
	}
	if !ok || f < 0.001 || f > 5 {
		return nil, nil, nil, errors.New("improve: budget_usd: out of range 0.001..5")
	}
	return j.BudgetUSD, f, scalar("!!float", strconv.FormatFloat(f, 'f', -1, 64)), nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

func scalar(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// setKey replaces or appends key in a YAML mapping node.
func setKey(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			val.HeadComment, val.LineComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, scalar("!!str", key), val)
}
