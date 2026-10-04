// Package config loads and validates job definitions (one YAML file per job).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/vincent/tarea/internal/fsx"
)

// Defaults applied when a field is omitted.
const (
	DefaultMaxSteps  = 8
	DefaultBudgetUSD = 0.10
	DefaultMemoryKB  = 64
	DefaultMemoryFil = "memory.md"
)

// Job is one scheduled LLM job.
type Job struct {
	Name      string      `yaml:"-"`
	Enabled   *bool       `yaml:"enabled"`
	Schedule  string      `yaml:"schedule"`
	Model     string      `yaml:"model"`
	Fallbacks []string    `yaml:"fallbacks"`
	BudgetUSD float64     `yaml:"budget_usd"`
	MaxSteps  int         `yaml:"max_steps"`
	MaxTokens int         `yaml:"max_tokens"`
	Prompt    string      `yaml:"prompt"`
	MCP       []MCPServer `yaml:"mcp"`
	Memory    Memory      `yaml:"memory"`
	Sinks     []Sink      `yaml:"sinks"`
}

// BuiltinHTTP names the in-process HTTP tool (see mcpx/httptool).
const BuiltinHTTP = "http"

// MCPServer describes one MCP server a job may use: a stdio command, a remote
// HTTP endpoint, or an in-process builtin. Allow is mandatory: use ["*"] to
// expose every tool. For the http builtin, Hosts lists the reachable hosts and
// Headers are sent with every request.
type MCPServer struct {
	Name    string            `yaml:"name"`
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Builtin string            `yaml:"builtin"`
	Hosts   []string          `yaml:"hosts"`
	Allow   []string          `yaml:"allow"`
}

// Memory configures the per-job memory file.
type Memory struct {
	File  string `yaml:"file"`
	MaxKB int    `yaml:"max_kb"`
}

// Sink configures one output target; Options carries sink-specific keys.
type Sink struct {
	Type    string            `yaml:"type"`
	Options map[string]string `yaml:",inline"`
}

// IsEnabled reports whether the job should be scheduled (default true).
func (j Job) IsEnabled() bool { return j.Enabled == nil || *j.Enabled }

// Parse decodes, expands, defaults and validates one job definition.
// Unknown YAML keys are an error. Expansion applies to MCP command/env/url/
// headers and sink options, never to the prompt.
func Parse(name string, data []byte, lookup Lookup) (Job, error) {
	var j Job
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&j); err != nil && !errors.Is(err, io.EOF) {
		return Job{}, fmt.Errorf("job %s: parse yaml: %w", name, err)
	}
	j.Name = name

	if err := j.expand(lookup); err != nil {
		return Job{}, fmt.Errorf("job %s: %w", name, err)
	}
	j.applyDefaults()
	if err := j.Validate(); err != nil {
		return Job{}, err
	}
	return j, nil
}

func (j *Job) applyDefaults() {
	if j.MaxSteps == 0 {
		j.MaxSteps = DefaultMaxSteps
	}
	if j.BudgetUSD == 0 {
		j.BudgetUSD = DefaultBudgetUSD
	}
	if j.Memory.File == "" {
		j.Memory.File = DefaultMemoryFil
	}
	if j.Memory.MaxKB == 0 {
		j.Memory.MaxKB = DefaultMemoryKB
	}
}

func (j *Job) expand(lookup Lookup) error {
	var errs []error
	exp := func(field, s string) string {
		out, err := Expand(s, lookup)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field, err))
		}
		return out
	}
	expMap := func(field string, m map[string]string) {
		for k, v := range m {
			m[k] = exp(field+"."+k, v)
		}
	}

	for i := range j.MCP {
		s := &j.MCP[i]
		for k := range s.Command {
			s.Command[k] = exp(fmt.Sprintf("mcp[%d].command[%d]", i, k), s.Command[k])
		}
		for k := range s.Hosts {
			s.Hosts[k] = exp(fmt.Sprintf("mcp[%d].hosts[%d]", i, k), s.Hosts[k])
		}
		s.URL = exp(fmt.Sprintf("mcp[%d].url", i), s.URL)
		expMap(fmt.Sprintf("mcp[%d].env", i), s.Env)
		expMap(fmt.Sprintf("mcp[%d].headers", i), s.Headers)
	}
	for i := range j.Sinks {
		expMap(fmt.Sprintf("sinks[%d]", i), j.Sinks[i].Options)
	}
	return errors.Join(errs...)
}

// Validate checks semantic constraints and reports every problem at once.
func (j Job) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("job %s: "+format, append([]any{j.Name}, args...)...))
	}

	if !fsx.ValidName(j.Name) {
		add("invalid name %q", j.Name)
	}
	if _, err := cron.ParseStandard(j.Schedule); err != nil {
		add("schedule %q: %v", j.Schedule, err)
	}
	if strings.TrimSpace(j.Model) == "" {
		add("model is required")
	}
	if strings.TrimSpace(j.Prompt) == "" {
		add("prompt is required")
	}
	if j.BudgetUSD < 0 {
		add("budget_usd must be positive")
	}
	if j.MaxTokens < 0 {
		add("max_tokens must be >= 0")
	}
	if j.MaxSteps < 1 {
		add("max_steps must be >= 1")
	}
	if j.Memory.MaxKB < 1 || !fsx.ValidName(j.Memory.File) {
		add("memory: invalid file %q or max_kb %d", j.Memory.File, j.Memory.MaxKB)
	}
	errs = append(errs, j.validateMCP()...)
	if len(j.Sinks) == 0 {
		add("at least one sink is required")
	}
	for i, s := range j.Sinks {
		if s.Type == "" {
			add("sinks[%d].type is required", i)
		}
	}
	return errors.Join(errs...)
}

func (j Job) validateMCP() []error {
	var errs []error
	seen := make(map[string]bool, len(j.MCP))
	for i, s := range j.MCP {
		add := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("job %s: mcp[%d]: "+format, append([]any{j.Name, i}, args...)...))
		}
		switch {
		case !fsx.ValidName(s.Name) || strings.Contains(s.Name, "__"):
			add("invalid name %q (no separators, no \"__\")", s.Name)
		case seen[s.Name]:
			add("duplicate name %q", s.Name)
		}
		seen[s.Name] = true

		transports := 0
		for _, set := range []bool{len(s.Command) > 0, s.URL != "", s.Builtin != ""} {
			if set {
				transports++
			}
		}
		if transports != 1 {
			add("exactly one of command, url or builtin is required")
		}
		if s.Builtin != "" && s.Builtin != BuiltinHTTP {
			add("unknown builtin %q (supported: %s)", s.Builtin, BuiltinHTTP)
		}
		if s.Builtin == BuiltinHTTP && len(s.Hosts) == 0 {
			add("hosts is required for the http builtin")
		}
		if len(s.Allow) == 0 {
			add("allow is required (use [\"*\"] to expose every tool)")
		}
	}
	return errs
}
