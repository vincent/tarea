package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vincent/agentd/internal/llm"
)

// Built-in tool names. MCP tools are namespaced "server__tool", so these never collide.
const (
	toolMemoryAppend  = "memory_append"
	toolMemoryReplace = "memory_replace"
	toolSeenCheck     = "seen_check"
	toolSeenAdd       = "seen_add"
)

type builtins struct {
	mem  Memory
	seen Seen
}

func (b builtins) defs() []llm.ToolDef {
	var out []llm.ToolDef
	if b.mem != nil {
		out = append(out,
			llm.ToolDef{
				Name:        toolMemoryAppend,
				Description: "Append one durable note to your long-lived memory.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}},"required":["note"]}`),
			},
			llm.ToolDef{
				Name:        toolMemoryReplace,
				Description: "Replace your whole memory with a rewritten, more compact version.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}`),
			},
		)
	}
	if b.seen != nil {
		keysSchema := json.RawMessage(`{"type":"object","properties":{"keys":{"type":"array","items":{"type":"string"}}},"required":["keys"]}`)
		out = append(out,
			llm.ToolDef{
				Name:        toolSeenCheck,
				Description: "Given stable item keys (e.g. URLs or ids), return which were already reported.",
				Parameters:  keysSchema,
			},
			llm.ToolDef{
				Name:        toolSeenAdd,
				Description: "Mark item keys as reported so they are never reported again.",
				Parameters:  keysSchema,
			},
		)
	}
	return out
}

// handle runs a built-in tool. handled=false means the name is not an active built-in.
func (b builtins) handle(name string, args json.RawMessage) (out string, handled bool, err error) {
	var fn func(json.RawMessage) (string, error)
	switch {
	case name == toolMemoryAppend && b.mem != nil:
		fn = b.memoryAppend
	case name == toolMemoryReplace && b.mem != nil:
		fn = b.memoryReplace
	case name == toolSeenCheck && b.seen != nil:
		fn = b.seenCheck
	case name == toolSeenAdd && b.seen != nil:
		fn = b.seenAdd
	default:
		return "", false, nil
	}
	out, err = fn(args)
	return out, true, err
}

func (b builtins) memoryAppend(args json.RawMessage) (string, error) {
	var a struct {
		Note string `json:"note"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Note) == "" {
		return "", errors.New("note must not be empty")
	}
	return "ok", b.mem.Append(a.Note)
}

func (b builtins) memoryReplace(args json.RawMessage) (string, error) {
	var a struct {
		Content string `json:"content"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	return "ok", b.mem.Replace(a.Content)
}

func (b builtins) seenCheck(args json.RawMessage) (string, error) {
	keys, err := decodeKeys(args)
	if err != nil {
		return "", err
	}
	res := struct {
		Seen   []string `json:"seen"`
		Unseen []string `json:"unseen"`
	}{Seen: []string{}, Unseen: []string{}}
	for _, k := range keys {
		if b.seen.Has(k) {
			res.Seen = append(res.Seen, k)
		} else {
			res.Unseen = append(res.Unseen, k)
		}
	}
	raw, err := json.Marshal(res)
	return string(raw), err
}

func (b builtins) seenAdd(args json.RawMessage) (string, error) {
	keys, err := decodeKeys(args)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("added %d key(s)", len(keys)), b.seen.Add(keys...)
}

func decodeKeys(args json.RawMessage) ([]string, error) {
	var a struct {
		Keys []string `json:"keys"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if len(a.Keys) == 0 {
		return nil, errors.New("keys must not be empty")
	}
	return a.Keys, nil
}

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
