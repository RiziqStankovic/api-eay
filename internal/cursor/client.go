package cursor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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
	TokenProfile        string
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
	PreflightAuth(ctx context.Context) error
	ForceRefreshAuth(ctx context.Context) error
	SwitchAuthProfile() bool
	ActiveAuthProfile() string
	ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (types.ChatCompletionResponse, error)
	ChatCompletionStream(ctx context.Context, req types.ChatCompletionRequest, onChunk func(delta string) error) (types.Usage, error)
	ResponsesStream(ctx context.Context, req types.ChatCompletionRequest, onEvent func(StreamEvent) error) (types.Usage, error)
}

type StreamEvent struct {
	Type        string
	Delta       string
	ToolCall    *types.ToolCall
	OutputIndex int
	Done        bool
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

func (c *client) PreflightAuth(ctx context.Context) error {
	if c.tokens == nil {
		return nil
	}
	_, err := c.tokens.authorization(ctx, false)
	return err
}

func (c *client) ForceRefreshAuth(ctx context.Context) error {
	if c.tokens == nil || !c.tokens.canRefresh() {
		return nil
	}
	_, err := c.tokens.authorization(ctx, true)
	return err
}

func (c *client) SwitchAuthProfile() bool {
	if c.tokens == nil {
		return false
	}
	return c.tokens.switchToNextProfile()
}

func (c *client) ActiveAuthProfile() string {
	if c.tokens == nil {
		return ""
	}
	return c.tokens.activeProfile()
}

func (c *client) ChatCompletion(ctx context.Context, req types.ChatCompletionRequest) (types.ChatCompletionResponse, error) {
	if err := c.ValidateModel(req.Model); err != nil {
		return types.ChatCompletionResponse{}, err
	}

	var out strings.Builder
	var toolCalls []types.ToolCall
	usage, err := c.streamFromUpstream(ctx, req, func(event StreamEvent) error {
		if event.Delta != "" {
			out.WriteString(event.Delta)
		}
		if event.ToolCall != nil {
			toolCalls = upsertToolCall(toolCalls, *event.ToolCall)
		}
		return nil
	})
	if err != nil {
		return types.ChatCompletionResponse{}, err
	}

	now := time.Now().Unix()
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	resp := types.ChatCompletionResponse{
		ID:      "chatcmpl-customai",
		Object:  "chat.completion",
		Created: now,
		Model:   req.Model,
		Choices: []types.ChatCompletionResponseChoice{
			{
				Index: 0,
				Message: types.ChatCompletionMessage{
					Role:      "assistant",
					Content:   types.MessageContent{Text: out.String()},
					ToolCalls: toolCalls,
				},
				FinishReason: finishReason,
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
	return c.streamFromUpstream(ctx, req, func(event StreamEvent) error {
		if event.Delta == "" {
			return nil
		}
		return onChunk(event.Delta)
	})
}

func (c *client) ResponsesStream(ctx context.Context, req types.ChatCompletionRequest, onEvent func(StreamEvent) error) (types.Usage, error) {
	if err := c.ValidateModel(req.Model); err != nil {
		return types.Usage{}, err
	}
	return c.streamFromUpstream(ctx, req, onEvent)
}

func (c *client) streamFromUpstream(ctx context.Context, req types.ChatCompletionRequest, onEvent func(StreamEvent) error) (types.Usage, error) {
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
	trackingOnEvent := func(event StreamEvent) error {
		wroteDelta = true
		return onEvent(event)
	}

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		usage, err := c.doUpstreamRequest(ctx, req, payload, false, trackingOnEvent)
		if err == nil {
			return usage, nil
		}
		if errors.Is(err, ErrTokenExpired) && c.tokens.canRefresh() {
			log.Printf("[auth] request_id=%s upstream token expired, attempting refresh", reqID)
			usage, refreshErr := c.doUpstreamRequest(ctx, req, payload, true, trackingOnEvent)
			if refreshErr == nil {
				return usage, nil
			}
			err = refreshErr
		}
		if isAuthProfileFailure(err) && !wroteDelta {
			for c.SwitchAuthProfile() {
				log.Printf("[auth] request_id=%s switching to fallback token profile profile=%q", reqID, c.ActiveAuthProfile())
				usage, fallbackErr := c.doUpstreamRequest(ctx, req, payload, false, trackingOnEvent)
				if fallbackErr == nil {
					return usage, nil
				}
				err = fallbackErr
				if errors.Is(err, ErrTokenExpired) && c.tokens.canRefresh() {
					log.Printf("[auth] request_id=%s fallback token expired, attempting refresh profile=%q", reqID, c.ActiveAuthProfile())
					usage, refreshErr := c.doUpstreamRequest(ctx, req, payload, true, trackingOnEvent)
					if refreshErr == nil {
						return usage, nil
					}
					err = refreshErr
				}
				if !isAuthProfileFailure(err) || wroteDelta {
					break
				}
			}
			if isAuthProfileFailure(err) {
				log.Printf("[auth] request_id=%s no fallback token profile available after auth failure: %v", reqID, err)
			}
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

func (c *client) doUpstreamRequest(ctx context.Context, req types.ChatCompletionRequest, payload []byte, forceRefresh bool, onEvent func(StreamEvent) error) (types.Usage, error) {
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
	if accountID := chatGPTAccountIDFromToken(token); accountID != "" {
		httpReq.Header.Set("ChatGPT-Account-Id", accountID)
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

		event, parsedUsage, err := parseStreamData(data)
		if err != nil {
			return types.Usage{}, err
		}
		if parsedUsage != (types.Usage{}) {
			usage = parsedUsage
		}
		if event.Type != "" && !event.Done {
			if err := onEvent(event); err != nil {
				return types.Usage{}, err
			}
		}
		if event.Done {
			return usage, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return types.Usage{}, err
	}
	return usage, nil
}

func isAuthProfileFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTokenExpired) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "refresh_token_reused") ||
		strings.Contains(msg, "invalid_client") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "token refresh failed")
}

func chatGPTAccountIDFromToken(token string) string {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return ""
	}
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		if accountID, ok := auth["chatgpt_account_id"].(string); ok {
			return strings.TrimSpace(accountID)
		}
	}
	if accountID, ok := payload["https://api.openai.com/auth.chatgpt_account_id"].(string); ok {
		return strings.TrimSpace(accountID)
	}
	if accountID, ok := payload["chatgpt_account_id"].(string); ok {
		return strings.TrimSpace(accountID)
	}
	return ""
}

type upstreamRequest struct {
	Model             string `json:"model"`
	Instructions      string `json:"instructions,omitempty"`
	Input             []any  `json:"input"`
	Stream            bool   `json:"stream"`
	Store             bool   `json:"store"`
	Tools             []any  `json:"tools,omitempty"`
	ToolChoice        any    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool  `json:"parallel_tool_calls,omitempty"`
}

func buildUpstreamRequest(req types.ChatCompletionRequest, stream bool, defaultInstructions string) upstreamRequest {
	var instructions []string
	input := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := strings.TrimSpace(strings.ToLower(m.Role))
		text := strings.TrimSpace(m.Content.PlainText())
		if role == "assistant" && len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				input = append(input, upstreamFunctionCallInput(tc))
			}
			if text == "" && !m.Content.HasImage() {
				continue
			}
		}
		if role == "tool" {
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": firstNonEmpty(m.ToolCallID, m.Name),
				"output":  m.Content.PlainText(),
			})
			continue
		}
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
		Model:             req.Model,
		Instructions:      joinedInstructions,
		Input:             input,
		Stream:            stream,
		Store:             false,
		Tools:             normalizeToolsForResponses(req.Tools),
		ToolChoice:        req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
	}
}

func upstreamFunctionCallInput(tc types.ToolCall) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"call_id":   firstNonEmpty(tc.ID, "call_"+requestid.New()),
		"name":      tc.Function.Name,
		"arguments": firstNonEmpty(tc.Function.Arguments, "{}"),
	}
}

func normalizeToolsForResponses(tools []types.ToolDefinition) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		toolType := strings.TrimSpace(tool.Type)
		if toolType == "" {
			toolType = "function"
		}
		name := strings.TrimSpace(tool.Name)
		description := tool.Description
		parameters := tool.Parameters
		if name == "" && strings.TrimSpace(tool.Function.Name) != "" {
			name = strings.TrimSpace(tool.Function.Name)
			description = tool.Function.Description
			parameters = tool.Function.Parameters
		}
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"type":        toolType,
			"name":        name,
			"description": description,
			"parameters":  parameters,
		})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
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

func parseStreamData(data string) (StreamEvent, types.Usage, error) {
	var raw struct {
		Type        string `json:"type"`
		Delta       string `json:"delta"`
		OutputIndex int    `json:"output_index"`
		ItemID      string `json:"item_id"`
		Item        *struct {
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
		} `json:"item,omitempty"`
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
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return StreamEvent{}, types.Usage{}, nil
	}

	switch raw.Type {
	case "response.output_text.delta":
		return StreamEvent{Type: raw.Type, Delta: raw.Delta, OutputIndex: raw.OutputIndex}, types.Usage{}, nil
	case "response.output_item.added", "response.output_item.done":
		if raw.Item == nil || raw.Item.Type != "function_call" {
			return StreamEvent{}, types.Usage{}, nil
		}
		args := raw.Item.Arguments
		if raw.Type == "response.output_item.added" && args == "{}" {
			args = ""
		}
		return StreamEvent{
			Type:        raw.Type,
			OutputIndex: raw.OutputIndex,
			ToolCall: &types.ToolCall{
				Index: intPtr(raw.OutputIndex),
				ID:    firstNonEmpty(raw.Item.CallID, raw.Item.ID),
				Type:  "function",
				Function: types.ToolCallFunction{
					Name:      raw.Item.Name,
					Arguments: args,
				},
			},
		}, types.Usage{}, nil
	case "response.function_call_arguments.delta":
		return StreamEvent{
			Type:        raw.Type,
			OutputIndex: raw.OutputIndex,
			ToolCall: &types.ToolCall{
				Index: intPtr(raw.OutputIndex),
				ID:    raw.ItemID,
				Type:  "function",
				Function: types.ToolCallFunction{
					Arguments: raw.Delta,
				},
			},
		}, types.Usage{}, nil
	case "response.completed":
		usage := types.Usage{}
		if raw.Response != nil && raw.Response.Usage != nil {
			usage = types.Usage{
				PromptTokens:     raw.Response.Usage.InputTokens,
				CompletionTokens: raw.Response.Usage.OutputTokens,
				TotalTokens:      raw.Response.Usage.TotalTokens,
			}
		}
		return StreamEvent{Type: raw.Type, Done: true}, usage, nil
	case "response.failed", "response.error", "error":
		if raw.Error != nil && raw.Error.Code == "token_expired" {
			return StreamEvent{}, types.Usage{}, ErrTokenExpired
		}
		if raw.Response != nil && raw.Response.Error != nil && raw.Response.Error.Code == "token_expired" {
			return StreamEvent{}, types.Usage{}, ErrTokenExpired
		}
		if raw.Error != nil && raw.Error.Message != "" {
			return StreamEvent{}, types.Usage{}, markTransientIfRetryableMessage(errors.New(raw.Error.Message))
		}
		if raw.Response != nil && raw.Response.Error != nil && raw.Response.Error.Message != "" {
			return StreamEvent{}, types.Usage{}, markTransientIfRetryableMessage(errors.New(raw.Response.Error.Message))
		}
		return StreamEvent{}, types.Usage{}, markTransientIfRetryableMessage(fmt.Errorf("upstream error: %s", raw.Type))
	default:
		return StreamEvent{}, types.Usage{}, nil
	}
}

func intPtr(v int) *int {
	return &v
}

func upsertToolCall(calls []types.ToolCall, call types.ToolCall) []types.ToolCall {
	if strings.TrimSpace(call.Type) == "" {
		call.Type = "function"
	}
	if strings.TrimSpace(call.Function.Arguments) == "" {
		call.Function.Arguments = "{}"
	}
	for i := range calls {
		if sameToolCall(calls[i], call) {
			if call.Function.Name != "" {
				calls[i].Function.Name = call.Function.Name
			}
			if call.Function.Arguments != "" && call.Function.Arguments != "{}" {
				calls[i].Function.Arguments += call.Function.Arguments
			}
			return calls
		}
	}
	return append(calls, call)
}

func sameToolCall(a, b types.ToolCall) bool {
	if a.Index != nil && b.Index != nil {
		return *a.Index == *b.Index
	}
	return a.ID != "" && a.ID == b.ID
}
