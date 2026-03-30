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
	"strings"
	"time"

	"github.com/openclaw/customai-gateway-go/internal/cursor"
	"github.com/openclaw/customai-gateway-go/internal/types"
)

type ChatCompletionsHandler struct {
	customClient cursor.Client
}

func NewChatCompletionsHandler(c cursor.Client) http.Handler {
	return &ChatCompletionsHandler{customClient: c}
}

type chatRequestBody struct {
	Model        string                        `json:"model"`
	Messages     []types.ChatCompletionMessage `json:"messages"`
	Stream       bool                          `json:"stream,omitempty"`
	Instructions string                        `json:"instructions,omitempty"`
	Input        []types.AltInputItem          `json:"input,omitempty"`
	Store        bool                          `json:"store,omitempty"`
}

func normalizeRequest(raw *chatRequestBody) (types.ChatCompletionRequest, bool) {
	if strings.TrimSpace(raw.Model) == "" {
		return types.ChatCompletionRequest{}, false
	}
	if len(raw.Messages) > 0 {
		return types.ChatCompletionRequest{Model: raw.Model, Messages: raw.Messages, Stream: raw.Stream}, true
	}
	alt := types.AltChatRequest{Model: raw.Model, Instructions: raw.Instructions, Input: raw.Input, Stream: raw.Stream}
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
		log.Printf("[gateway] invalid request JSON: %v body=%q", err, preview)
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
		log.Printf("customai ChatCompletion error: %v", err)
		writeError(w, http.StatusBadGateway, "customai backend error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode response error: %v", err)
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

	err := h.customClient.ChatCompletionStream(ctx, req, func(delta string) error {
		chunk := types.ChatCompletionStreamChunk{
			ID:      chunkID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []types.ChatCompletionStreamChunkChoice{
				{
					Index: 0,
					Delta: types.ChatCompletionStreamChunkDelta{Content: delta},
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
		log.Printf("customai ChatCompletionStream error: %v", err)
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		if _, writeErr := bw.WriteString("event: error\ndata: " + string(payload) + "\n\n"); writeErr != nil {
			log.Printf("write stream error event: %v", writeErr)
		}
		_ = bw.Flush()
		flusher.Flush()
	}

	stop := "stop"
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
			log.Printf("write final chunk error: %v", writeErr)
		}
	}
	if _, err := bw.WriteString("data: [DONE]\n\n"); err != nil {
		log.Printf("write [DONE] error: %v", err)
	}
	_ = bw.Flush()
	flusher.Flush()
}

func (h *ChatCompletionsHandler) handleResponsesNonStream(ctx context.Context, w http.ResponseWriter, req types.ChatCompletionRequest) {
	resp, err := h.customClient.ChatCompletion(ctx, req)
	if err != nil {
		log.Printf("customai Responses non-stream error: %v", err)
		writeError(w, http.StatusBadGateway, "customai backend error")
		return
	}
	text := ""
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content.Text
	}

	payload := map[string]any{
		"id":      "resp_" + randomID(),
		"object":  "response",
		"created_at": time.Now().Unix(),
		"status":  "completed",
		"model":   req.Model,
		"parallel_tool_calls": true,
		"tool_choice": "auto",
		"tools": []any{},
		"top_p": 1.0,
		"output": []any{
			map[string]any{
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": text,
					},
				},
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *ChatCompletionsHandler) handleResponsesStream(ctx context.Context, w http.ResponseWriter, req types.ChatCompletionRequest) {
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
			"id":         respID,
			"object":     "response",
			"created_at": createdAt,
			"status":     "in_progress",
			"model":      req.Model,
			"output":     []any{},
			"parallel_tool_calls": true,
			"tool_choice": "auto",
			"tools":      []any{},
			"top_p":      1.0,
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

	err := h.customClient.ChatCompletionStream(ctx, req, func(delta string) error {
		return writeEvent(map[string]any{
			"type":          "response.output_text.delta",
			"delta":         delta,
			"item_id":       itemID,
			"output_index":  0,
			"content_index": 0,
		})
	})
	if err != nil {
		log.Printf("customai Responses stream error: %v", err)
		_ = writeEvent(map[string]any{
			"type": "error",
			"error": map[string]any{
				"message": err.Error(),
			},
		})
	} else {
		_ = writeEvent(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":                  respID,
				"object":              "response",
				"created_at":          createdAt,
				"status":              "completed",
				"model":               req.Model,
				"output":              []any{},
				"parallel_tool_calls": true,
				"tool_choice":         "auto",
				"tools":               []any{},
				"top_p":               1.0,
			},
		})
	}

	if _, doneErr := bw.WriteString("data: [DONE]\n\n"); doneErr != nil {
		log.Printf("write [DONE] error: %v", doneErr)
	}
	_ = bw.Flush()
	flusher.Flush()
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "customai"
	}
	return hex.EncodeToString(b)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(types.ErrorResponse{
		Error: types.ErrorBody{Message: msg, Type: "customai-gateway-error"},
	})
}

func isResponsesPath(path string) bool {
	return path == "/responses" || path == "/v1/responses"
}
