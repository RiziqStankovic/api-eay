package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/customai-gateway-go/internal/types"
)

var ErrInvalidModel = errors.New("invalid model")

type Config struct {
	APIURL        string
	AuthToken     string
	Cookie        string
	RequestTTL    time.Duration
	ExtraHeaders  map[string]string
	DefaultInstructions string
	LogPayload bool
	PayloadLogMaxChars int
	AllowedModels []string
}

type Client interface {
	ValidateModel(model string) error
	ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (types.ChatCompletionResponse, error)
	ChatCompletionStream(ctx context.Context, req types.ChatCompletionRequest, onChunk func(delta string) error) (types.Usage, error)
}

type client struct {
	cfg        Config
	httpClient *http.Client
}

func NewClient(cfg Config) Client {
	ttl := cfg.RequestTTL
	if ttl <= 0 {
		ttl = 180 * time.Second
	}
	return &client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: ttl,
		},
	}
}

func (c *client) ValidateModel(model string) error {
	if len(c.cfg.AllowedModels) == 0 {
		return nil
	}
	model = strings.TrimSpace(model)
	for _, allowed := range c.cfg.AllowedModels {
		if strings.TrimSpace(allowed) == model {
			return nil
		}
	}
	return ErrInvalidModel
}

func (c *client) ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (types.ChatCompletionResponse, error) {
	if err := c.ValidateModel(req.Model); err != nil {
		return types.ChatCompletionResponse{}, err
	}

	var out strings.Builder
	usage, err := c.streamFromUpstream(ctx, req, func(delta string) error {
		out.WriteString(delta)
		return nil
	})
	if err != nil {
		return types.ChatCompletionResponse{}, err
	}

	now := time.Now().Unix()
	resp := types.ChatCompletionResponse{
		ID:      "chatcmpl-customai",
		Object:  "chat.completion",
		Created: now,
		Model:   req.Model,
		Choices: []types.ChatCompletionResponseChoice{
			{
				Index: 0,
				Message: types.ChatCompletionMessage{
					Role:    "assistant",
					Content: types.MessageContent{Text: out.String()},
				},
				FinishReason: "stop",
			},
		},
		Usage: usage,
	}
	return resp, nil
}

func (c *client) ChatCompletionStream(ctx context.Context, req types.ChatCompletionRequest, onChunk func(delta string) error) (types.Usage, error) {
	if err := c.ValidateModel(req.Model); err != nil {
		return types.Usage{}, err
	}
	return c.streamFromUpstream(ctx, req, onChunk)
}

func (c *client) streamFromUpstream(ctx context.Context, req types.ChatCompletionRequest, onChunk func(delta string) error) (types.Usage, error) {
	if strings.TrimSpace(c.cfg.APIURL) == "" {
		return types.Usage{}, fmt.Errorf("CUSTOMAI_API_URL is required")
	}

	upBody := buildUpstreamRequest(req, true, c.cfg.DefaultInstructions)
	payload, err := json.Marshal(upBody)
	if err != nil {
		return types.Usage{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(payload))
	if err != nil {
		return types.Usage{}, err
	}
	if c.cfg.LogPayload {
		maxChars := c.cfg.PayloadLogMaxChars
		if maxChars <= 0 {
			maxChars = 4000
		}
		log.Printf("[upstream] payload=%s", truncateForLog(string(payload), maxChars))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if token := normalizeBearer(c.cfg.AuthToken); token != "" {
		httpReq.Header.Set("Authorization", token)
	}
	if strings.TrimSpace(c.cfg.Cookie) != "" {
		httpReq.Header.Set("Cookie", strings.TrimSpace(c.cfg.Cookie))
	}
	for k, v := range c.cfg.ExtraHeaders {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		httpReq.Header.Set(k, v)
	}
	log.Printf(
		"[upstream] method=%s url=%s headers=%s model=%q stream=%v",
		httpReq.Method,
		httpReq.URL.String(),
		redactedHeaders(httpReq.Header),
		req.Model,
		true,
	)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return types.Usage{}, err
	}
	defer resp.Body.Close()
	log.Printf("[upstream] status=%d url=%s", resp.StatusCode, httpReq.URL.String())

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return types.Usage{}, fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	usage := types.Usage{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		delta, done, parsedUsage, err := parseStreamData(data)
		if err != nil {
			return types.Usage{}, err
		}
		if parsedUsage != (types.Usage{}) {
			usage = parsedUsage
		}
		if delta != "" {
			if err := onChunk(delta); err != nil {
				return types.Usage{}, err
			}
		}
		if done {
			return usage, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return types.Usage{}, err
	}
	return usage, nil
}

type upstreamRequest struct {
	Model        string               `json:"model"`
	Instructions string               `json:"instructions,omitempty"`
	Input        []types.AltInputItem `json:"input"`
	Stream       bool                 `json:"stream"`
	Store        bool                 `json:"store"`
}

func buildUpstreamRequest(req types.ChatCompletionRequest, stream bool, defaultInstructions string) upstreamRequest {
	var instructions []string
	input := make([]types.AltInputItem, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := strings.TrimSpace(strings.ToLower(m.Role))
		text := strings.TrimSpace(m.Content.Text)
		if text == "" {
			continue
		}
		if role == "system" {
			instructions = append(instructions, text)
			continue
		}
		if role == "" {
			role = "user"
		}
		input = append(input, types.AltInputItem{
			Type:    "message",
			Role:    role,
			Content: types.MessageContent{Text: text},
		})
	}
	if len(input) == 0 {
		input = append(input, types.AltInputItem{
			Type:    "message",
			Role:    "user",
			Content: types.MessageContent{Text: ""},
		})
	}
	joinedInstructions := strings.TrimSpace(strings.Join(instructions, "\n\n"))
	if joinedInstructions == "" {
		joinedInstructions = strings.TrimSpace(defaultInstructions)
	}
	if joinedInstructions == "" {
		joinedInstructions = "You are a helpful coding assistant."
	}
	return upstreamRequest{
		Model:        req.Model,
		Instructions: joinedInstructions,
		Input:        input,
		Stream:       stream,
		Store:        false,
	}
}

func redactedHeaders(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "cookie" || lk == "set-cookie" || lk == "x-api-key" {
			parts = append(parts, k+"=<redacted>")
			continue
		}
		parts = append(parts, k+"="+strings.Join(h.Values(k), ","))
	}
	return strings.Join(parts, "; ")
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func normalizeBearer(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	return "Bearer " + token
}

func parseStreamData(data string) (delta string, done bool, usage types.Usage, err error) {
	var event struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
		Response *struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage,omitempty"`
		} `json:"response,omitempty"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return "", false, types.Usage{}, nil
	}

	switch event.Type {
	case "response.output_text.delta":
		return event.Delta, false, types.Usage{}, nil
	case "response.completed":
		if event.Response != nil && event.Response.Usage != nil {
			usage = types.Usage{
				PromptTokens:     event.Response.Usage.InputTokens,
				CompletionTokens: event.Response.Usage.OutputTokens,
				TotalTokens:      event.Response.Usage.TotalTokens,
			}
		}
		return "", true, usage, nil
	case "response.failed", "response.error", "error":
		if event.Error != nil && event.Error.Message != "" {
			return "", false, types.Usage{}, fmt.Errorf(event.Error.Message)
		}
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
			return "", false, types.Usage{}, fmt.Errorf(event.Response.Error.Message)
		}
		return "", false, types.Usage{}, fmt.Errorf("upstream error: %s", event.Type)
	default:
		return "", false, types.Usage{}, nil
	}
}
