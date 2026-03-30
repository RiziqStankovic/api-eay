package types

import (
	"encoding/json"
	"strings"
)

// MessageContent handles OpenAI content: string or array of parts (text, image_url).
// Extracts text and ignores non-text parts.
type MessageContent struct {
	Text string
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Text = s
		return nil
	}
	var single struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &single); err == nil {
		c.Text = strings.TrimSpace(single.Text)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	var b strings.Builder
	for _, p := range parts {
		pt := strings.ToLower(strings.TrimSpace(p.Type))
		if (pt == "text" || pt == "input_text" || pt == "output_text") && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	c.Text = b.String()
	return nil
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.Text)
}

// ChatCompletionRequest is a minimal subset of the OpenAI Chat Completions API
// compatible with what OpenClaw expects for provider api=openai-completions.
type ChatCompletionRequest struct {
	Model            string                  `json:"model"`
	Messages         []ChatCompletionMessage `json:"messages"`
	Stream           bool                    `json:"stream,omitempty"`
	Temperature      *float32                `json:"temperature,omitempty"`
	MaxTokens        *int                    `json:"max_tokens,omitempty"`
	TopP             *float32                `json:"top_p,omitempty"`
	FrequencyPenalty *float32                `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float32                `json:"presence_penalty,omitempty"`
	// Extra fields are ignored for now.
}

// AltChatRequest is an alternate body format (e.g. instructions + input array).
// Used when the client sends { "instructions", "input" } instead of "messages".
type AltChatRequest struct {
	Model         string         `json:"model"`
	Instructions  string         `json:"instructions,omitempty"`
	Input         []AltInputItem `json:"input,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	Store         bool           `json:"store,omitempty"`
}

// AltInputItem is one entry in the "input" array (type "message" with role + content).
type AltInputItem struct {
	Type    string         `json:"type,omitempty"` // e.g. "message"
	Role    string         `json:"role,omitempty"` // user, assistant, system
	Content MessageContent `json:"content,omitempty"`
}

// ToChatCompletionRequest converts AltChatRequest to ChatCompletionRequest.
// Instructions become a system message; each input item with type "message" becomes a message.
func (a AltChatRequest) ToChatCompletionRequest() ChatCompletionRequest {
	var messages []ChatCompletionMessage
	if a.Instructions != "" {
		messages = append(messages, ChatCompletionMessage{Role: "system", Content: MessageContent{Text: a.Instructions}})
	}
	for _, item := range a.Input {
		itemType := strings.TrimSpace(strings.ToLower(item.Type))
		// Responses API often omits item.type; treat role-bearing item as a message.
		if itemType != "" && itemType != "message" {
			continue
		}
		if item.Role == "" {
			continue
		}
		messages = append(messages, ChatCompletionMessage{Role: item.Role, Content: MessageContent{Text: item.Content.Text}})
	}
	return ChatCompletionRequest{
		Model:    a.Model,
		Messages: messages,
		Stream:   a.Stream,
	}
}

type ChatCompletionMessage struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// ChatCompletionResponse mirrors the non-streaming OpenAI response shape.
type ChatCompletionResponse struct {
	ID      string                         `json:"id"`
	Object  string                         `json:"object"`
	Created int64                          `json:"created"`
	Model   string                         `json:"model"`
	Choices []ChatCompletionResponseChoice `json:"choices"`
	Usage   Usage                          `json:"usage"`
}

type ChatCompletionResponseChoice struct {
	Index        int                          `json:"index"`
	Message      ChatCompletionMessage        `json:"message"`
	FinishReason string                       `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionStreamChunk is used for SSE / streaming responses.
type ChatCompletionStreamChunk struct {
	ID      string                             `json:"id"`
	Object  string                             `json:"object"`
	Created int64                              `json:"created"`
	Model   string                             `json:"model"`
	Choices []ChatCompletionStreamChunkChoice  `json:"choices"`
}

type ChatCompletionStreamChunkChoice struct {
	Index int                              `json:"index"`
	Delta ChatCompletionStreamChunkDelta   `json:"delta"`
	// FinishReason is usually only set on the final chunk.
	FinishReason *string                   `json:"finish_reason,omitempty"`
}

type ChatCompletionStreamChunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ErrorResponse is a minimal OpenAI-compatible error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}
