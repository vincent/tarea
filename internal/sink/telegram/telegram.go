// Package telegram delivers messages through the Telegram Bot API using plain
// HTTPS (no client library). Text is sent as-is, without parse_mode, so model
// output can never break message parsing.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vincent/tarea/internal/sink"
)

const (
	defaultBaseURL = "https://api.telegram.org"
	// MaxChunkRunes stays below Telegram's 4096-character limit.
	MaxChunkRunes = 4000
	maxBodyBytes  = 1 << 20
)

// Config configures a Sink.
type Config struct {
	Token      string
	ChatID     string
	BaseURL    string
	HTTP       *http.Client
	MaxRetries int
	Sleep      func(ctx context.Context, d time.Duration) error
}

// Sink sends messages to one chat.
type Sink struct{ cfg Config }

// New validates cfg and applies defaults.
func New(cfg Config) (*Sink, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram: bot token is required (set TELEGRAM_BOT_TOKEN)")
	}
	if cfg.ChatID == "" {
		return nil, errors.New("telegram: chat_id is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	return &Sink{cfg: cfg}, nil
}

// Factory returns a sink.Factory using a default bot token. A job may override
// the token or set chat_id through its sink options.
func Factory(defaultToken string, client *http.Client) sink.Factory {
	return func(opts map[string]string) (sink.Sink, error) {
		token := defaultToken
		if t := opts["token"]; t != "" {
			token = t
		}
		return New(Config{Token: token, ChatID: opts["chat_id"], HTTP: client})
	}
}

// Send delivers the message, split into as many chunks as needed.
func (s *Sink) Send(ctx context.Context, m sink.Message) error {
	chunks := Chunk(m.Text, MaxChunkRunes)
	if len(chunks) == 0 {
		return errors.New("telegram: empty message")
	}
	for i, c := range chunks {
		if err := s.sendOne(ctx, c); err != nil {
			return fmt.Errorf("telegram: chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}
	return nil
}

// SendAudio uploads m.Audio (MP3) as an audio message captioned with the job.
func (s *Sink) SendAudio(ctx context.Context, m sink.Message) error {
	if len(m.Audio) == 0 {
		return errors.New("telegram: empty audio")
	}
	for attempt := 0; ; attempt++ {
		body, ctype, err := audioBody(s.cfg.ChatID, m)
		if err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
		resp, status, err := s.postTo(ctx, "sendAudio", ctype, body)
		if err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
		if resp.OK {
			return nil
		}
		if status == http.StatusTooManyRequests && attempt < s.cfg.MaxRetries {
			wait := time.Duration(max(resp.Parameters.RetryAfter, 1)) * time.Second
			if err = s.cfg.Sleep(ctx, wait); err != nil {
				return fmt.Errorf("telegram: %w", err)
			}
			continue
		}
		return fmt.Errorf("telegram: api error (status %d): %s", status, resp.Description)
	}
}

func audioBody(chatID string, m sink.Message) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err = w.WriteField("chat_id", chatID); err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	if err = w.WriteField("title", m.Job); err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	part, err := w.CreateFormFile("audio", "response.mp3")
	if err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	if _, err = part.Write(m.Audio); err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	if err = w.Close(); err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (s *Sink) sendOne(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  s.cfg.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	for attempt := 0; ; attempt++ {
		resp, status, err := s.postTo(ctx, "sendMessage", "application/json", body)
		if err != nil {
			return err
		}
		if resp.OK {
			return nil
		}
		if status == http.StatusTooManyRequests && attempt < s.cfg.MaxRetries {
			wait := time.Duration(max(resp.Parameters.RetryAfter, 1)) * time.Second
			if err = s.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("api error (status %d): %s", status, resp.Description)
	}
}

func (s *Sink) postTo(ctx context.Context, method, ctype string, body []byte) (apiResponse, int, error) {
	endpoint := fmt.Sprintf("%s/bot%s/%s", strings.TrimRight(s.cfg.BaseURL, "/"), s.cfg.Token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return apiResponse{}, 0, errors.New("build request failed")
	}
	req.Header.Set("Content-Type", ctype)

	res, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return apiResponse{}, 0, redact(err) // *url.Error embeds the URL, which contains the token.
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))
	if err != nil {
		return apiResponse{}, 0, fmt.Errorf("read response: %w", err)
	}
	var out apiResponse
	if err = json.Unmarshal(raw, &out); err != nil {
		return apiResponse{}, res.StatusCode, fmt.Errorf("decode response (status %d): %w", res.StatusCode, err)
	}
	return out, res.StatusCode, nil
}

func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("request failed: %w", ue.Err)
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("sleep: %w", ctx.Err())
	case <-t.C:
		return nil
	}
}

// Chunk splits text into pieces of at most limit runes, preferring paragraph,
// then line, then word boundaries, and hard-splitting only as a last resort.
func Chunk(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" || limit < 1 {
		return nil
	}
	return pack(text, limit, []string{"\n\n", "\n", " "})
}

func pack(text string, limit int, seps []string) []string {
	if runeLen(text) <= limit {
		return []string{text}
	}
	if len(seps) == 0 {
		return hardSplit(text, limit)
	}

	sep := seps[0]
	var (
		out []string
		cur string
	)
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, part := range strings.Split(text, sep) {
		if runeLen(part) > limit {
			flush()
			out = append(out, pack(part, limit, seps[1:])...)
			continue
		}
		cand := part
		if cur != "" {
			cand = cur + sep + part
		}
		if runeLen(cand) <= limit {
			cur = cand
			continue
		}
		flush()
		cur = part
	}
	flush()
	return out
}

func hardSplit(text string, limit int) []string {
	r := []rune(text)
	out := make([]string, 0, len(r)/limit+1)
	for len(r) > limit {
		out = append(out, string(r[:limit]))
		r = r[limit:]
	}
	return append(out, string(r))
}

func runeLen(s string) int { return len([]rune(s)) }
