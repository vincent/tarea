package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincent/tarea/internal/llm"
)

func newClient(url string) (*llm.OpenRouter, *[]time.Duration) {
	c := llm.NewOpenRouter("test-key")
	c.BaseURL = url
	var sleeps []time.Duration
	c.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	return c, &sleeps
}

func TestChat_ToolCallResponseAndRequestShape(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile("testdata/tool_call_response.json")
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Path != "/chat/completions" {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	c, _ := newClient(srv.URL)
	resp, err := c.Chat(context.Background(), llm.Request{
		Model:     "a/b",
		Fallbacks: []string{"c/d"},
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Tools:     []llm.ToolDef{{Name: "events__search", Description: "d"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Name != "events__search" {
		t.Fatalf("tool calls: %+v", resp.Message.ToolCalls)
	}
	if resp.Usage.CostUSD != 0.0021 || resp.Usage.PromptTokens != 120 || resp.Model != "openai/gpt-5-mini" {
		t.Fatalf("usage/model: %+v %q", resp.Usage, resp.Model)
	}
	models, _ := got["models"].([]any)
	if len(models) != 2 || models[0] != "a/b" || models[1] != "c/d" {
		t.Fatalf("models fallback list: %v", got["models"])
	}
	if usage, _ := got["usage"].(map[string]any); usage["include"] != true {
		t.Fatalf("usage accounting not requested: %v", got["usage"])
	}
}

func TestChat_RetriesOn429HonoringRetryAfter(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, sleeps := newClient(srv.URL)
	resp, err := c.Chat(context.Background(), llm.Request{Model: "m"})
	if err != nil || resp.Message.Content != "ok" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 7*time.Second {
		t.Fatalf("sleeps = %v", *sleeps)
	}
}

func TestChat_ExhaustsRetriesOn5xx(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	c, sleeps := newClient(srv.URL)
	c.MaxRetries = 2
	_, err := c.Chat(context.Background(), llm.Request{Model: "m"})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 3 || len(*sleeps) != 2 || (*sleeps)[1] != 2*(*sleeps)[0] {
		t.Fatalf("calls=%d sleeps=%v", calls.Load(), *sleeps)
	}
}

func TestChat_NoRetryOn4xx(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := newClient(srv.URL)
	_, err := c.Chat(context.Background(), llm.Request{Model: "m"})
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestChat_MalformedAndErrorBodies(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"malformed":  `{"choices":`,
		"no choices": `{"choices":[]}`,
		"error body": `{"error":{"code":402,"message":"insufficient credits"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			c, _ := newClient(srv.URL)
			if _, err := c.Chat(context.Background(), llm.Request{Model: "m"}); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestChat_ContextCancelled(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release) // runs first: unblocks the handler so Close() cannot hang.

	c, _ := newClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Chat(ctx, llm.Request{Model: "m"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestUsageAdd(t *testing.T) {
	t.Parallel()
	got := llm.Usage{PromptTokens: 1, CompletionTokens: 2, CostUSD: 0.5}.Add(llm.Usage{PromptTokens: 3, CompletionTokens: 4, CostUSD: 0.25})
	if got != (llm.Usage{PromptTokens: 4, CompletionTokens: 6, CostUSD: 0.75}) {
		t.Fatalf("got %+v", got)
	}
}

func TestChat_MaxTokensAndFinishReason(t *testing.T) {
	t.Parallel()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil // Decode merges into an existing map: start clean per request.
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"cut"}}]}`))
	}))
	defer srv.Close()

	c, _ := newClient(srv.URL)
	resp, err := c.Chat(context.Background(), llm.Request{Model: "m", MaxTokens: 256, Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(256) || resp.FinishReason != "length" {
		t.Fatalf("max_tokens=%v finish=%q", got["max_tokens"], resp.FinishReason)
	}

	if _, err = c.Chat(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["max_tokens"]; ok {
		t.Fatal("max_tokens must be omitted when unset")
	}
}

func TestSpeech_RequestShapeAndAudio(t *testing.T) {
	t.Parallel()
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Path != "/audio/speech" {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3audio"))
	}))
	defer srv.Close()

	c, _ := newClient(srv.URL)
	audio, err := c.Speech(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(audio) != "ID3audio" {
		t.Fatalf("audio = %q", audio)
	}
	if got["model"] != llm.SpeechModel || got["input"] != "hello" || got["response_format"] != "mp3" {
		t.Fatalf("body = %v", got)
	}
}

func TestSpeech_RetriesOn429(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("mp3"))
	}))
	defer srv.Close()

	c, sleeps := newClient(srv.URL)
	if _, err := c.Speech(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(*sleeps) != 1 || (*sleeps)[0] != 2*time.Second {
		t.Fatalf("calls=%d sleeps=%v", calls.Load(), *sleeps)
	}
}

func TestSpeech_BadResponses(t *testing.T) {
	t.Parallel()
	tests := map[string]string{"empty": "", "json error": `{"error":{"message":"nope"}}`}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c, _ := newClient(srv.URL)
			if _, err := c.Speech(context.Background(), "x"); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
