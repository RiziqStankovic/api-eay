package http

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openclaw/customai-gateway-go/internal/cursor"
	"github.com/openclaw/customai-gateway-go/internal/requestid"
	"github.com/openclaw/customai-gateway-go/internal/types"
)

type ChatCompletionsHandler struct {
	customClient cursor.Client
}

func NewChatCompletionsHandler(c cursor.Client) http.Handler {
	return &ChatCompletionsHandler{customClient: c}
}

type chatRequestBody struct {
	Model             string                        `json:"model"`
	Messages          []types.ChatCompletionMessage `json:"messages"`
	Stream            bool                          `json:"stream,omitempty"`
	Instructions      string                        `json:"instructions,omitempty"`
	Input             []types.AltInputItem          `json:"input,omitempty"`
	Store             bool                          `json:"store,omitempty"`
	Tools             []types.ToolDefinition        `json:"tools,omitempty"`
	ToolChoice        any                           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                         `json:"parallel_tool_calls,omitempty"`
	Reasoning         any                           `json:"reasoning,omitempty"`
	ReasoningEffort   string                        `json:"reasoning_effort,omitempty"`
	Thinking          any                           `json:"thinking,omitempty"`
}

func normalizeRequest(raw *chatRequestBody) (types.ChatCompletionRequest, bool) {
	if strings.TrimSpace(raw.Model) == "" {
		return types.ChatCompletionRequest{}, false
	}
	if len(raw.Messages) > 0 {
		return types.ChatCompletionRequest{
			Model:             raw.Model,
			Messages:          raw.Messages,
			Stream:            raw.Stream,
			Tools:             raw.Tools,
			ToolChoice:        raw.ToolChoice,
			ParallelToolCalls: raw.ParallelToolCalls,
			Reasoning:         raw.Reasoning,
			ReasoningEffort:   raw.ReasoningEffort,
			Thinking:          raw.Thinking,
		}, true
	}
	alt := types.AltChatRequest{
		Model:             raw.Model,
		Instructions:      raw.Instructions,
		Input:             raw.Input,
		Stream:            raw.Stream,
		Tools:             raw.Tools,
		ToolChoice:        raw.ToolChoice,
		ParallelToolCalls: raw.ParallelToolCalls,
		Reasoning:         raw.Reasoning,
		ReasoningEffort:   raw.ReasoningEffort,
		Thinking:          raw.Thinking,
	}
	return alt.ToChatCompletionRequest(), true
}

func (h *ChatCompletionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "empty request body")
		return
	}

	var raw chatRequestBody
	if err := json.Unmarshal(body, &raw); err != nil {
		preview := string(body)
		if len(preview) > 400 {
			preview = preview[:400] + "..."
		}
		log.Printf("[gateway] request_id=%s invalid request JSON: %v body=%q", requestid.FromContext(r.Context()), err, preview)
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	req, ok := normalizeRequest(&raw)
	if !ok {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages or (instructions/input) required")
		return
	}

	if err := h.customClient.ValidateModel(req.Model); err != nil {
		if errors.Is(err, cursor.ErrInvalidModel) {
			writeError(w, http.StatusBadRequest, "invalid model")
			return
		}
	}

	if req.Stream {
		if isResponsesPath(r.URL.Path) {
			h.handleResponsesStream(r.Context(), w, req)
			return
		}
		h.handleStream(r.Context(), w, req)
		return
	}
	if isResponsesPath(r.URL.Path) {
		h.handleResponsesNonStream(r.Context(), w, req)
		return
	}
	h.handleNonStream(r.Context(), w, req)
}

func (h *ChatCompletionsHandler) handleNonStream(ctx context.Context, w http.ResponseWriter, req types.ChatCompletionRequest) {
	resp, err := h.customClient.ChatCompletion(ctx, req)
	if err != nil {
		log.Printf("customai ChatCompletion error request_id=%s: %v", requestid.FromContext(ctx), err)
		writeError(w, http.StatusBadGateway, "customai backend error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode response error request_id=%s: %v", requestid.FromContext(ctx), err)
	}
}

func (h *ChatCompletionsHandler) handleStream(ctx context.Context, w http.ResponseWriter, req types.ChatCompletionRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	bw := bufio.NewWriter(w)
	chunkID := "chatcmpl-" + randomID()
	created := time.Now().Unix()

	wroteToolCall := false
	toolCallBuffers := map[string]types.ToolCall{}
	_, err := h.customClient.ResponsesStream(ctx, req, func(event cursor.StreamEvent) error {
		delta := types.ChatCompletionStreamChunkDelta{}
		if event.Delta != "" {
			delta.Content = event.Delta
		}
		if event.ReasoningDelta != "" {
			delta.ReasoningContent = event.ReasoningDelta
		}
		if event.ToolCall != nil {
			wroteToolCall = true
			toolCall := *event.ToolCall
			if toolCall.Type == "" {
				toolCall.Type = "function"
			}
			if toolCall.Index == nil {
				toolCall.Index = intPtr(0)
			}
			toolKey := toolCallStreamKey(toolCall)
			buffered := toolCallBuffers[toolKey]
			if event.Type == "response.output_item.done" && buffered.Function.Arguments != "" {
				toolCall.Function.Arguments = ""
			}
			buffered = mergeBufferedToolCall(buffered, toolCall)
			toolCallBuffers[toolKey] = buffered
			switch event.Type {
			case "response.output_item.done":
				buffered.Function.Arguments = normalizeToolArguments(buffered.Function.Name, buffered.Function.Arguments)
				delta.ToolCalls = []types.ToolCall{buffered}
				delete(toolCallBuffers, toolKey)
			default:
				// Zed/ACP executes tool calls from the streamed chunk it sees.
				// Buffer partial added/arguments events so it receives one complete call.
			}
		}
		if delta.Content == "" && delta.ReasoningContent == "" && len(delta.ToolCalls) == 0 {
			return nil
		}
		chunk := types.ChatCompletionStreamChunk{
			ID:      chunkID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []types.ChatCompletionStreamChunkChoice{
				{
					Index: 0,
					Delta: delta,
				},
			},
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := bw.WriteString("data: " + string(data) + "\n\n"); err != nil {
			return err
		}
		_ = bw.Flush()
		flusher.Flush()
		return nil
	})
	if err != nil {
		log.Printf("customai ChatCompletionStream error request_id=%s: %v", requestid.FromContext(ctx), err)
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		if _, writeErr := bw.WriteString("event: error\ndata: " + string(payload) + "\n\n"); writeErr != nil {
			log.Printf("write stream error event request_id=%s: %v", requestid.FromContext(ctx), writeErr)
		}
		_ = bw.Flush()
		flusher.Flush()
	}

	stop := "stop"
	if wroteToolCall {
		stop = "tool_calls"
	}
	finalChunk := types.ChatCompletionStreamChunk{
		ID:      chunkID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   req.Model,
		Choices: []types.ChatCompletionStreamChunkChoice{
			{
				Index:        0,
				Delta:        types.ChatCompletionStreamChunkDelta{},
				FinishReason: &stop,
			},
		},
	}
	if data, marshalErr := json.Marshal(finalChunk); marshalErr == nil {
		if _, writeErr := bw.WriteString("data: " + string(data) + "\n\n"); writeErr != nil {
			log.Printf("write final chunk error request_id=%s: %v", requestid.FromContext(ctx), writeErr)
		}
	}
	if _, err := bw.WriteString("data: [DONE]\n\n"); err != nil {
		log.Printf("write [DONE] error request_id=%s: %v", requestid.FromContext(ctx), err)
	}
	_ = bw.Flush()
	flusher.Flush()
}

func toolCallStreamKey(toolCall types.ToolCall) string {
	if toolCall.Index != nil {
		return "index:" + strconv.Itoa(*toolCall.Index)
	}
	if strings.TrimSpace(toolCall.ID) != "" {
		return strings.TrimSpace(toolCall.ID)
	}
	return "index:0"
}

func mergeBufferedToolCall(dst, src types.ToolCall) types.ToolCall {
	if dst.Index == nil {
		dst.Index = src.Index
	}
	if dst.ID == "" {
		dst.ID = src.ID
	}
	if dst.Type == "" {
		dst.Type = firstNonEmpty(src.Type, "function")
	}
	if src.Function.Name != "" {
		dst.Function.Name = src.Function.Name
	}
	if src.Function.Arguments != "" {
		dst.Function.Arguments += src.Function.Arguments
	}
	return dst
}

func normalizeToolArguments(toolName, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return "{}"
	}

	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	cleaned := cleanToolArgumentValue(value)
	if cleaned == nil {
		return "{}"
	}
	if obj, ok := cleaned.(map[string]any); ok {
		cleanToolArgumentObject(strings.TrimSpace(toolName), obj)
		if len(obj) == 0 {
			return "{}"
		}
		cleaned = obj
	}
	data, err := json.Marshal(cleaned)
	if err != nil {
		return raw
	}
	return string(data)
}

func cleanToolArgumentValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			cleaned := cleanToolArgumentValue(child)
			if cleaned == nil {
				continue
			}
			out[key] = cleaned
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			cleaned := cleanToolArgumentValue(child)
			if cleaned != nil {
				out = append(out, cleaned)
			}
		}
		return out
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" || strings.EqualFold(trimmed, "null") || strings.EqualFold(trimmed, ".null") {
			return nil
		}
		return v
	default:
		return v
	}
}

func cleanToolArgumentObject(toolName string, obj map[string]any) {
	switch toolName {
	case "find_path":
		if glob, ok := obj["glob"].(string); ok {
			if normalized := normalizeFindPathGlob(glob); normalized != "" {
				obj["glob"] = normalized
			}
		}
	case "diagnostics":
		delete(obj, "directory")
		for _, key := range []string{"path", "file"} {
			if isBadDiagnosticsPath(obj[key]) {
				delete(obj, key)
			}
		}
	}
}

func normalizeFindPathGlob(raw string) string {
	glob := strings.TrimSpace(strings.ReplaceAll(raw, `\`, `/`))
	if glob == "" {
		return ""
	}
	wildcard := strings.IndexAny(glob, "*?[")
	if wildcard < 0 {
		return strings.TrimPrefix(glob, "./")
	}

	prefix := strings.TrimRight(glob[:wildcard], "/")
	suffix := glob[wildcard:]
	if !isAbsoluteLikePath(prefix) {
		return strings.TrimPrefix(glob, "./")
	}

	parts := strings.Split(prefix, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part == "" || strings.HasSuffix(part, ":") {
			continue
		}
		return part + "/" + suffix
	}
	return strings.TrimPrefix(glob, "./")
}

func isAbsoluteLikePath(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasPrefix(path, "/") ||
		(len(path) >= 2 && path[1] == ':') ||
		strings.HasPrefix(path, "//")
}

func isBadDiagnosticsPath(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(s)
	return trimmed == "" ||
		trimmed == "/" ||
		strings.EqualFold(trimmed, "null") ||
		strings.EqualFold(trimmed, ".null") ||
		!looksLikeFilePath(trimmed)
}

func looksLikeFilePath(path string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), `/\`)
	if path == "" {
		return false
	}
	lastSlash := strings.LastIndexAny(path, `/\`)
	base := path
	if lastSlash >= 0 {
		base = path[lastSlash+1:]
	}
	return strings.Contains(base, ".")
}

func (h *ChatCompletionsHandler) handleResponsesNonStream(ctx context.Context, w http.ResponseWriter, req types.ChatCompletionRequest) {
	resp, err := h.customClient.ChatCompletion(ctx, req)
	if err != nil {
		log.Printf("customai Responses non-stream error request_id=%s: %v", requestid.FromContext(ctx), err)
		writeError(w, http.StatusBadGateway, "customai backend error")
		return
	}
	text := ""
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content.Text
	}
	itemID := "msg_" + randomID()

	payload := map[string]any{
		"id":                  "resp_" + randomID(),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               req.Model,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"top_p":               1.0,
		"output": []any{
			map[string]any{
				"id":     itemID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{
					map[string]any{
						"type":        "output_text",
						"text":        text,
						"annotations": []any{},
					},
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
			"total_tokens":  resp.Usage.TotalTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *ChatCompletionsHandler) handleResponsesStream(ctx context.Context, w http.ResponseWriter, req types.ChatCompletionRequest) {
	if err := h.customClient.PreflightAuth(ctx); err != nil {
		log.Printf("customai Responses preflight auth error request_id=%s: %v", requestid.FromContext(ctx), err)
		writeErrorWithCode(w, http.StatusBadGateway, err.Error(), inferResponsesErrorCode(err.Error()))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported by server")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	bw := bufio.NewWriter(w)
	respID := "resp_" + randomID()
	itemID := "msg_" + randomID()
	createdAt := time.Now().Unix()
	var fullText strings.Builder
	toolCalls := make([]types.ToolCall, 0)

	writeEvent := func(v map[string]any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := bw.WriteString("data: " + string(data) + "\n\n"); err != nil {
			return err
		}
		_ = bw.Flush()
		flusher.Flush()
		return nil
	}

	_ = writeEvent(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":                  respID,
			"object":              "response",
			"created_at":          createdAt,
			"status":              "in_progress",
			"model":               req.Model,
			"output":              []any{},
			"parallel_tool_calls": true,
			"tool_choice":         "auto",
			"tools":               []any{},
			"top_p":               1.0,
		},
	})
	_ = writeEvent(map[string]any{
		"type": "response.output_item.added",
		"item": map[string]any{
			"id":      itemID,
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
		"output_index": 0,
	})
	_ = writeEvent(map[string]any{
		"type":          "response.content_part.added",
		"item_id":       itemID,
		"output_index":  0,
		"content_index": 0,
		"part": map[string]any{
			"type": "output_text",
			"text": "",
		},
	})

	usage, err := h.customClient.ResponsesStream(ctx, req, func(event cursor.StreamEvent) error {
		if event.Delta != "" {
			fullText.WriteString(event.Delta)
			return writeEvent(map[string]any{
				"type":          "response.output_text.delta",
				"delta":         event.Delta,
				"item_id":       itemID,
				"output_index":  0,
				"content_index": 0,
			})
		}
		if event.ToolCall == nil {
			return nil
		}
		toolCall := *event.ToolCall
		if toolCall.ID == "" {
			toolCall.ID = "call_" + randomID()
		}
		if toolCall.Type == "" {
			toolCall.Type = "function"
		}
		toolCalls = upsertResponseToolCall(toolCalls, toolCall)
		callIndex := len(toolCalls)
		switch event.Type {
		case "response.function_call_arguments.delta":
			return writeEvent(map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      toolCall.ID,
				"output_index": callIndex,
				"delta":        toolCall.Function.Arguments,
			})
		default:
			return writeEvent(map[string]any{
				"type":         "response.output_item.added",
				"output_index": callIndex,
				"item": map[string]any{
					"id":        toolCall.ID,
					"type":      "function_call",
					"call_id":   toolCall.ID,
					"name":      toolCall.Function.Name,
					"arguments": toolCall.Function.Arguments,
					"status":    "completed",
				},
			})
		}
	})
	if err != nil {
		log.Printf("customai Responses stream error request_id=%s: %v", requestid.FromContext(ctx), err)
		_ = writeEvent(buildResponsesErrorEvent(err))
	} else {
		output := []any{
			map[string]any{
				"id":     itemID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []any{
					map[string]any{
						"type":        "output_text",
						"text":        fullText.String(),
						"annotations": []any{},
					},
				},
			},
		}
		for _, toolCall := range toolCalls {
			toolCall.Function.Arguments = normalizeToolArguments(toolCall.Function.Name, toolCall.Function.Arguments)
			output = append(output, map[string]any{
				"id":        toolCall.ID,
				"type":      "function_call",
				"call_id":   toolCall.ID,
				"name":      toolCall.Function.Name,
				"arguments": firstNonEmpty(toolCall.Function.Arguments, "{}"),
				"status":    "completed",
			})
		}
		_ = writeEvent(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":                  respID,
				"object":              "response",
				"created_at":          createdAt,
				"status":              "completed",
				"model":               req.Model,
				"output":              output,
				"parallel_tool_calls": true,
				"tool_choice":         "auto",
				"tools":               responseTools(req.Tools),
				"top_p":               1.0,
				"usage": map[string]any{
					"input_tokens":  usage.PromptTokens,
					"output_tokens": usage.CompletionTokens,
					"total_tokens":  usage.TotalTokens,
				},
			},
		})
	}

	if _, doneErr := bw.WriteString("data: [DONE]\n\n"); doneErr != nil {
		log.Printf("write [DONE] error request_id=%s: %v", requestid.FromContext(ctx), doneErr)
	}
	_ = bw.Flush()
	flusher.Flush()
}

func upsertResponseToolCall(calls []types.ToolCall, call types.ToolCall) []types.ToolCall {
	for i := range calls {
		if sameResponseToolCall(calls[i], call) {
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

func sameResponseToolCall(a, b types.ToolCall) bool {
	if a.Index != nil && b.Index != nil {
		return *a.Index == *b.Index
	}
	return a.ID != "" && a.ID == b.ID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func intPtr(v int) *int {
	return &v
}

func responseTools(tools []types.ToolDefinition) []any {
	if len(tools) == 0 {
		return []any{}
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
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
			"type":        firstNonEmpty(tool.Type, "function"),
			"name":        name,
			"description": description,
			"parameters":  parameters,
		})
	}
	if out == nil {
		return []any{}
	}
	return out
}

func buildResponsesErrorEvent(err error) map[string]any {
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	code := inferResponsesErrorCode(message)
	return map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
		"param":   nil,
		"error": map[string]any{
			"code":    code,
			"message": message,
			"param":   nil,
		},
	}
}

func inferResponsesErrorCode(message string) string {
	msg := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(msg, "invalid_client"):
		return "invalid_client"
	case strings.Contains(msg, "refresh_token_reused"):
		return "invalid_client"
	case strings.Contains(msg, "token expired"), strings.Contains(msg, "token_expired"):
		return "token_expired"
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "timeout"):
		return "timeout"
	default:
		return "gateway_error"
	}
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "customai"
	}
	return hex.EncodeToString(b)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeErrorWithCode(w, status, msg, "")
}

func writeErrorWithCode(w http.ResponseWriter, status int, msg, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(types.ErrorResponse{
		Error: types.ErrorBody{Message: msg, Type: "customai-gateway-error", Code: code},
	})
}

func isResponsesPath(path string) bool {
	return path == "/responses" || path == "/v1/responses"
}
