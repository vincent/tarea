package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the OpenRouter API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

const (
	maxResponseBytes = 8 << 20
	maxAudioBytes    = 20 << 20

	// SpeechModel is the OpenRouter text-to-speech model.
	SpeechModel  = "fish-audio/s2.1-pro-free:free"
	speechFormat = "mp3"
)

// APIError is a non-retryable (or exhausted) provider error.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openrouter: status %d: %s", e.Status, e.Message)
}

// OpenRouter is a minimal OpenAI-compatible chat client for openrouter.ai.
// The zero value is not usable: build it with NewOpenRouter.
type OpenRouter struct {
	APIKey     string
	BaseURL    string
	HTTP       *http.Client
	MaxRetries int                                              // extra attempts on 429/5xx.
	Backoff    time.Duration                                    // base delay, doubled per attempt.
	Sleep      func(ctx context.Context, d time.Duration) error // injectable for tests.
	Referer    string
	Title      string
}

// NewOpenRouter returns a client with sane defaults.
func NewOpenRouter(apiKey string) *OpenRouter {
	return &OpenRouter{
		APIKey:     apiKey,
		BaseURL:    DefaultBaseURL,
		HTTP:       &http.Client{Timeout: 2 * time.Minute},
		MaxRetries: 3,
		Backoff:    time.Second,
		Sleep:      sleepCtx,
		Title:      "tarea",
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("sleep: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// Chat performs one completion, retrying on 429 and 5xx with backoff.
func (c *OpenRouter) Chat(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(c.buildBody(req))
	if err != nil {
		return Response{}, fmt.Errorf("openrouter: encode request: %w", err)
	}
	raw, err := c.withRetry(ctx, "/chat/completions", body, maxResponseBytes)
	if err != nil {
		return Response{}, err
	}
	return parseResponse(raw)
}

// Speech synthesizes text to MP3 audio with the free Fish Audio model.
func (c *OpenRouter) Speech(ctx context.Context, text string) ([]byte, error) {
	body, err := json.Marshal(map[string]string{
		"model":           SpeechModel,
		"input":           text,
		"response_format": speechFormat,
	})
	if err != nil {
		return nil, fmt.Errorf("openrouter: encode request: %w", err)
	}
	raw, err := c.withRetry(ctx, "/audio/speech", body, maxAudioBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errors.New("openrouter: empty audio response")
	}
	// Audio is binary; JSON means an error payload with a 200.
	if t := bytes.TrimLeft(raw, "\ufeff \t\r\n"); len(t) > 0 && t[0] == '{' {
		return nil, fmt.Errorf("openrouter: speech returned JSON: %s", snippet(raw))
	}
	return raw, nil
}

// withRetry POSTs body to path, retrying on 429, 5xx and network errors.
func (c *OpenRouter) withRetry(ctx context.Context, path string, body []byte, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := c.Sleep(ctx, c.delay(attempt, lastErr)); err != nil {
				return nil, err
			}
		}
		raw, retry, err := c.do(ctx, path, body, limit)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("openrouter: giving up after %d retries: %w", c.MaxRetries, lastErr)
}

type retryAfterError struct {
	*APIError
	after time.Duration
}

// Unwrap lets errors.As find the embedded *APIError.
func (e *retryAfterError) Unwrap() error { return e.APIError }

func (c *OpenRouter) delay(attempt int, last error) time.Duration {
	var ra *retryAfterError
	if errors.As(last, &ra) && ra.after > 0 {
		return ra.after
	}
	return c.Backoff << (attempt - 1)
}

func (c *OpenRouter) do(ctx context.Context, path string, body []byte, limit int64) (raw []byte, retry bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("openrouter: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if c.Referer != "" {
		httpReq.Header.Set("HTTP-Referer", c.Referer)
	}
	if c.Title != "" {
		httpReq.Header.Set("X-Title", c.Title)
	}

	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("openrouter: %w", ctx.Err())
		}
		return nil, true, fmt.Errorf("openrouter: request: %w", err) // network error: retry.
	}
	defer func() { _ = res.Body.Close() }()

	raw, err = io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, true, fmt.Errorf("openrouter: read body: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, false, fmt.Errorf("openrouter: response exceeds %d bytes", limit)
	}

	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError {
		apiErr := &APIError{Status: res.StatusCode, Message: snippet(raw)}
		return nil, true, &retryAfterError{APIError: apiErr, after: parseRetryAfter(res.Header.Get("Retry-After"))}
	}
	if res.StatusCode != http.StatusOK {
		return nil, false, &APIError{Status: res.StatusCode, Message: snippet(raw)}
	}

	return raw, false, nil
}

func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// --- wire format -----------------------------------------------------------

type wireRequest struct {
	Model     string        `json:"model"`
	Models    []string      `json:"models,omitempty"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Usage     wireUsageOpt  `json:"usage"`
}

type wireUsageOpt struct {
	Include bool `json:"include"`
}

type wireMessage struct {
	Role       Role           `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireToolFuncer `json:"function"`
}

type wireToolFuncer struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string      `json:"finish_reason"`
		Message      wireMessage `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int     `json:"prompt_tokens"`
		CompletionTokens int     `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *OpenRouter) buildBody(req Request) wireRequest {
	w := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Usage: wireUsageOpt{Include: true}}
	if len(req.Fallbacks) > 0 {
		w.Models = append([]string{req.Model}, req.Fallbacks...)
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID: tc.ID, Type: "function",
				Function: wireFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		w.Tools = append(w.Tools, wireTool{
			Type:     "function",
			Function: wireToolFuncer{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	return w
}

func parseResponse(raw []byte) (Response, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return Response{}, fmt.Errorf("openrouter: decode response: %w (body: %s)", err, snippet(raw))
	}
	if w.Error != nil {
		return Response{}, &APIError{Status: http.StatusOK, Message: w.Error.Message}
	}
	if len(w.Choices) == 0 {
		return Response{}, errors.New("openrouter: response has no choices")
	}

	ch := w.Choices[0]
	msg := Message{Role: RoleAssistant, Content: ch.Message.Content}
	for _, tc := range ch.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	out := Response{Message: msg, Model: w.Model, FinishReason: ch.FinishReason}
	if w.Usage != nil {
		out.Usage = Usage{PromptTokens: w.Usage.PromptTokens, CompletionTokens: w.Usage.CompletionTokens, CostUSD: w.Usage.Cost}
	}
	return out, nil
}
