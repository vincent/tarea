package telegram_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent/agentd/internal/sink"
	"github.com/vincent/agentd/internal/sink/telegram"
)

func TestChunk(t *testing.T) {
	t.Parallel()
	para := strings.Repeat

	tests := []struct {
		name  string
		text  string
		limit int
		want  []string
	}{
		{"empty", "  \n ", 10, nil},
		{"fits", "hello", 10, []string{"hello"}},
		{"packs paragraphs", "aaaa\n\nbbbb\n\ncccc", 10, []string{"aaaa\n\nbbbb", "cccc"}},
		{"falls back to lines", "aaaa bbbb\ncccc dddd", 9, []string{"aaaa bbbb", "cccc dddd"}},
		{"falls back to words", "aaa bbb ccc", 7, []string{"aaa bbb", "ccc"}},
		{"hard split", para("x", 10), 4, []string{"xxxx", "xxxx", "xx"}},
		{"runes not bytes", para("é", 5), 3, []string{"ééé", "éé"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := telegram.Chunk(tt.text, tt.limit)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("got %q want %q", got, tt.want)
			}
			for _, c := range got {
				if len([]rune(c)) > tt.limit {
					t.Fatalf("chunk exceeds limit: %q", c)
				}
			}
		})
	}
}

type fakeAPI struct {
	mu      sync.Mutex
	texts   []string
	chatIDs []string
	status  []int // scripted statuses, popped per request; default 200.
}

func (f *fakeAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTOKEN/sendMessage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		status := http.StatusOK
		if len(f.status) > 0 {
			status, f.status = f.status[0], f.status[1:]
		}
		if status == http.StatusOK {
			f.texts = append(f.texts, body.Text)
			f.chatIDs = append(f.chatIDs, body.ChatID)
		}
		f.mu.Unlock()

		w.WriteHeader(status)
		switch status {
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case http.StatusTooManyRequests:
			_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":3}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
		}
	}
}

func newSink(t *testing.T, url string, sleeps *[]time.Duration) *telegram.Sink {
	t.Helper()
	s, err := telegram.New(telegram.Config{
		Token: "TOKEN", ChatID: "42", BaseURL: url,
		Sleep: func(_ context.Context, d time.Duration) error { *sleeps = append(*sleeps, d); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSend_ChunksLongMessages(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	var sleeps []time.Duration
	s := newSink(t, srv.URL, &sleeps)

	long := strings.Repeat("a", 3000) + "\n\n" + strings.Repeat("b", 3000)
	if err := s.Send(context.Background(), sink.Message{Job: "gigs", Text: long}); err != nil {
		t.Fatal(err)
	}
	if len(api.texts) != 2 || api.chatIDs[0] != "42" {
		t.Fatalf("texts=%d chat=%v", len(api.texts), api.chatIDs)
	}
}

func TestSend_RetriesOn429(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{status: []int{http.StatusTooManyRequests, http.StatusOK}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	var sleeps []time.Duration
	s := newSink(t, srv.URL, &sleeps)

	if err := s.Send(context.Background(), sink.Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 3*time.Second || len(api.texts) != 1 {
		t.Fatalf("sleeps=%v texts=%v", sleeps, api.texts)
	}
}

func TestSend_APIErrorAndEmpty(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{status: []int{http.StatusBadRequest}}
	srv := httptest.NewServer(api.handler(t))
	defer srv.Close()
	var sleeps []time.Duration
	s := newSink(t, srv.URL, &sleeps)

	if err := s.Send(context.Background(), sink.Message{Text: "hi"}); err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v", err)
	}
	if err := s.Send(context.Background(), sink.Message{Text: " "}); err == nil {
		t.Fatal("empty message must error")
	}
}

func TestSend_NetworkErrorDoesNotLeakToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused.
	var sleeps []time.Duration
	s := newSink(t, url, &sleeps)

	err := s.Send(context.Background(), sink.Message{Text: "hi"})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestNewValidationAndFactory(t *testing.T) {
	t.Parallel()
	if _, err := telegram.New(telegram.Config{ChatID: "1"}); err == nil {
		t.Fatal("token required")
	}
	if _, err := telegram.New(telegram.Config{Token: "t"}); err == nil {
		t.Fatal("chat_id required")
	}
	f := telegram.Factory("default", nil)
	if _, err := f(map[string]string{"chat_id": "9"}); err != nil {
		t.Fatalf("factory: %v", err)
	}
	if _, err := f(map[string]string{}); err == nil {
		t.Fatal("factory must require chat_id")
	}
}
