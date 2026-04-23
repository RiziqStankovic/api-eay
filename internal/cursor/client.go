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

	"github.com/openclaw/customai-gateway-go/internal/requestid"
	"github.com/openclaw/customai-gateway-go/internal/types"
)

var ErrInvalidModel = errors.New("invalid model")

type Config struct {
	APIURL              string
	AuthToken           string
	RefreshToken        string
	TokenURL            string
	OAuthClientID       string
	TokenScopes         []string
	TokenExpiresAt      time.Time
	RefreshBuffer       time.Duration
	TokenStorePath      string
	Cookie              string
	RequestTTL          time.Duration
	ExtraHeaders        map[string]string
	DefaultInstructions string
	LogPayload          bool
	PayloadLogMaxChars  int
	AllowedModels       []string
}

type Client interface {
	ValidateModel(model string) error
	ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (types.ChatCompletionResponse, error)
	ChatCompletionStream(ctx context.Context, req types.ChatCompletionRequest, onChunk func(delta string) error) (types.Usage, error)
}

type client struct {
	cfg        Config
	httpClient *http.Client
	tokens     *tokenManager
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
		tokens: newTokenManager(cfg),
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

	reqID := requestid.FromContext(ctx)
	upBody := buildUpstreamRequest(req, true, c.cfg.DefaultInstructions)
	payload, err := json.Marshal(upBody)
	if err != nil {
		return types.Usage{}, err
	}

	wroteDelta := false
	trackingOnChunk := func(delta string) error {
		wroteDelta = true
		return onChunk(delta)
	}

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		usage, err := c.doUpstreamRequest(ctx, req, payload, false, trackingOnChunk)
		if err == nil {
			return usage, nil
		}
		if errors.Is(err, ErrTokenExpired) && c.tokens.canRefresh() {
			log.Printf("[auth] request_id=%s upstream token expired, attempting refresh", reqID)
			return c.doUpstreamRequest(ctx, req, payload, true, trackingOnChunk)
		}
		if wroteDelta || !isTransientUpstreamError(err) || attempt == maxAttempts {
			return types.Usage{}, err
		}
		backoff := time.Duration(attempt) * 750 * time.Millisecond
		log.Printf("[upstream] request_id=%s transient error, retrying attempt=%d/%d delay_ms=%d error=%v", reqID, attempt+1, maxAttempts, backoff.Milliseconds(), err)
		if err := sleepContext(ctx, backoff); err != nil {
			return types.Usage{}, err
		}
	}
	return types.Usage{}, nil
}

func (c *client) doUpstreamRequest(ctx context.Context, req types.ChatCompletionRequest, payload []byte, forceRefresh bool, onChunk func(delta string) error) (types.Usage, error) {
	reqID := requestid.FromContext(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL, bytes.NewReader(payload))
	if err != nil {
		return types.Usage{}, err
	}
	if c.cfg.LogPayload {
		maxChars := c.cfg.PayloadLogMaxChars
		if maxChars <= 0 {
			maxChars = 4000
		}
		log.Printf("[upstream] request_id=%s payload=%s", reqID, truncateForLog(redactPayloadForLog(payload), maxChars))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if reqID != "" {
		httpReq.Header.Set("X-Request-Id", reqID)
	}
	token, err := c.tokens.authorization(ctx, forceRefresh)
	if err != nil {
		return types.Usage{}, err
	}
	if token != "" {
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
		"[upstream] request_id=%s method=%s url=%s headers=%s model=%q stream=%v",
		reqID,
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
	upstreamReqID := upstreamRequestID(resp.Header)
	log.Printf("[upstream] request_id=%s upstream_request_id=%s status=%d url=%s", reqID, upstreamReqID, resp.StatusCode, httpReq.URL.String())

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if resp.StatusCode == http.StatusUnauthorized || strings.Contains(string(raw), "token_expired") {
			return types.Usage{}, ErrTokenExpired
		}
		err := fmt.Errorf("upstream status %d upstream_request_id=%s: %s", resp.StatusCode, upstreamReqID, strings.TrimSpace(string(raw)))
		if isRetryableHTTPStatus(resp.StatusCode) {
			return types.Usage{}, transientUpstreamError{err: err}
		}
		return types.Usage{}, err
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
		text := strings.TrimSpace(m.Content.PlainText())
		if text == "" && !m.Content.HasImage() {
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
			Content: m.Content,
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

func upstreamRequestID(h http.Header) string {
	for _, key := range []string{"X-Request-Id", "Openai-Request-Id", "Request-Id", "X-Amzn-Requestid", "Cf-Ray"} {
		if value := strings.TrimSpace(h.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func redactPayloadForLog(payload []byte) string {
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return string(payload)
	}
	redactJSONValue(v)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return string(payload)
	}
	return strings.TrimSpace(b.String())
}

func redactJSONValue(v any) {
	switch value := v.(type) {
	case map[string]any:
		for k, child := range value {
			switch strings.ToLower(k) {
			case "image_url":
				value[k] = "<redacted image_url>"
			case "file_data":
				value[k] = "<redacted file_data>"
			default:
				redactJSONValue(child)
			}
		}
	case []any:
		for _, child := range value {
			redactJSONValue(child)
		}
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func isTransientUpstreamError(err error) bool {
	var transient transientUpstreamError
	if errors.As(err, &transient) {
		return true
	}
	return false
}

func markTransientIfRetryableMessage(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	retryableSnippets := []string{
		"overloaded",
		"please try again later",
		"retry your request",
		"temporarily unavailable",
		"rate limit",
	}
	for _, snippet := range retryableSnippets {
		if strings.Contains(msg, snippet) {
			return transientUpstreamError{err: err}
		}
	}
	return err
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
			Code    string `json:"code"`
		} `json:"error,omitempty"`
		Response *struct {
			Error *struct {
				Message string `json:"message"`
				Code    string `json:"code"`
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
		if event.Error != nil && event.Error.Code == "token_expired" {
			return "", false, types.Usage{}, ErrTokenExpired
		}
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Code == "token_expired" {
			return "", false, types.Usage{}, ErrTokenExpired
		}
		if event.Error != nil && event.Error.Message != "" {
			return "", false, types.Usage{}, markTransientIfRetryableMessage(errors.New(event.Error.Message))
		}
		if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
			return "", false, types.Usage{}, markTransientIfRetryableMessage(errors.New(event.Response.Error.Message))
		}
		return "", false, types.Usage{}, markTransientIfRetryableMessage(fmt.Errorf("upstream error: %s", event.Type))
	default:
		return "", false, types.Usage{}, nil
	}
}
