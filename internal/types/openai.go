package types

import (
	"encoding/json"
	"strings"
)

// MessageContent handles OpenAI content: string or multimodal content parts.
type MessageContent struct {
	Text  string
	Parts []MessageContentPart
}

type MessageContentPart struct {
	Type     string
	Text     string
	ImageURL string
	Detail   string
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	*c = MessageContent{}

	if strings.TrimSpace(string(data)) == "null" {
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Text = s
		return nil
	}

	var single rawContentPart
	if err := json.Unmarshal(data, &single); err == nil && strings.TrimSpace(single.Type) != "" {
		part := normalizeContentPart(single)
		c.Parts = appendPart(c.Parts, part)
		c.Text = partsText(c.Parts)
		return nil
	}

	var rawParts []rawContentPart
	if err := json.Unmarshal(data, &rawParts); err != nil {
		return err
	}
	for _, p := range rawParts {
		c.Parts = appendPart(c.Parts, normalizeContentPart(p))
	}
	c.Text = partsText(c.Parts)
	return nil
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if len(c.Parts) == 0 {
		return json.Marshal(c.Text)
	}
	out := make([]responseContentPart, 0, len(c.Parts))
	for _, part := range c.Parts {
		switch part.Type {
		case "input_text":
			if part.Text == "" {
				continue
			}
			out = append(out, responseContentPart{
				Type: "input_text",
				Text: part.Text,
			})
		case "input_image":
			if part.ImageURL == "" {
				continue
			}
			detail := strings.TrimSpace(part.Detail)
			if detail == "" {
				detail = "auto"
			}
			out = append(out, responseContentPart{
				Type:     "input_image",
				ImageURL: part.ImageURL,
				Detail:   detail,
			})
		}
	}
	if len(out) == 0 {
		return json.Marshal(c.Text)
	}
	if !hasImagePart(c.Parts) {
		return json.Marshal(c.Text)
	}
	return json.Marshal(out)
}

func (c MessageContent) PlainText() string {
	return c.Text
}

func (c MessageContent) HasImage() bool {
	return hasImagePart(c.Parts)
}

type rawContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL any    `json:"image_url"`
	Detail   string `json:"detail"`
}

type responseContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func normalizeContentPart(p rawContentPart) MessageContentPart {
	partType := strings.ToLower(strings.TrimSpace(p.Type))
	switch partType {
	case "text", "input_text":
		return MessageContentPart{Type: "input_text", Text: p.Text}
	case "image_url", "input_image":
		imageURL, detail := extractImageURL(p.ImageURL)
		if p.Detail != "" {
			detail = p.Detail
		}
		return MessageContentPart{
			Type:     "input_image",
			ImageURL: imageURL,
			Detail:   detail,
		}
	default:
		if p.Text != "" {
			return MessageContentPart{Type: "input_text", Text: p.Text}
		}
		return MessageContentPart{}
	}
}

func extractImageURL(v any) (string, string) {
	switch value := v.(type) {
	case string:
		return value, ""
	case map[string]any:
		detail, _ := value["detail"].(string)
		if url, ok := value["url"].(string); ok {
			return url, detail
		}
	}
	return "", ""
}

func appendPart(parts []MessageContentPart, part MessageContentPart) []MessageContentPart {
	if part.Type == "" {
		return parts
	}
	if part.Type == "input_text" && part.Text == "" {
		return parts
	}
	if part.Type == "input_image" && part.ImageURL == "" {
		return parts
	}
	return append(parts, part)
}

func partsText(parts []MessageContentPart) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type != "input_text" || part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func hasImagePart(parts []MessageContentPart) bool {
	for _, part := range parts {
		if part.Type == "input_image" && part.ImageURL != "" {
			return true
		}
	}
	return false
}

// ChatCompletionRequest is a minimal subset of the OpenAI Chat Completions API
// compatible with what OpenClaw expects for provider api=openai-completions.
type ChatCompletionRequest struct {
	Model             string                  `json:"model"`
	Messages          []ChatCompletionMessage `json:"messages"`
	Stream            bool                    `json:"stream,omitempty"`
	Temperature       *float32                `json:"temperature,omitempty"`
	MaxTokens         *int                    `json:"max_tokens,omitempty"`
	TopP              *float32                `json:"top_p,omitempty"`
	FrequencyPenalty  *float32                `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float32                `json:"presence_penalty,omitempty"`
	Tools             []ToolDefinition        `json:"tools,omitempty"`
	ToolChoice        any                     `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                   `json:"parallel_tool_calls,omitempty"`
	Reasoning         any                     `json:"reasoning,omitempty"`
	ReasoningEffort   string                  `json:"reasoning_effort,omitempty"`
	Thinking          any                     `json:"thinking,omitempty"`
	// Extra fields are ignored for now.
}

// AltChatRequest is an alternate body format (e.g. instructions + input array).
// Used when the client sends { "instructions", "input" } instead of "messages".
type AltChatRequest struct {
	Model             string           `json:"model"`
	Instructions      string           `json:"instructions,omitempty"`
	Input             []AltInputItem   `json:"input,omitempty"`
	Stream            bool             `json:"stream,omitempty"`
	Store             bool             `json:"store,omitempty"`
	Tools             []ToolDefinition `json:"tools,omitempty"`
	ToolChoice        any              `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
	Reasoning         any              `json:"reasoning,omitempty"`
	ReasoningEffort   string           `json:"reasoning_effort,omitempty"`
	Thinking          any              `json:"thinking,omitempty"`
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
		messages = append(messages, ChatCompletionMessage{Role: item.Role, Content: item.Content})
	}
	return ChatCompletionRequest{
		Model:             a.Model,
		Messages:          messages,
		Stream:            a.Stream,
		Tools:             a.Tools,
		ToolChoice:        a.ToolChoice,
		ParallelToolCalls: a.ParallelToolCalls,
		Reasoning:         a.Reasoning,
		ReasoningEffort:   a.ReasoningEffort,
		Thinking:          a.Thinking,
	}
}

type ChatCompletionMessage struct {
	Role             string         `json:"role"`
	Content          MessageContent `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
}

type ToolDefinition struct {
	Type        string       `json:"type"`
	Function    ToolFunction `json:"function,omitempty"`
	Name        string       `json:"name,omitempty"`
	Description string       `json:"description,omitempty"`
	Parameters  any          `json:"parameters,omitempty"`
}

type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
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
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionStreamChunk is used for SSE / streaming responses.
type ChatCompletionStreamChunk struct {
	ID      string                            `json:"id"`
	Object  string                            `json:"object"`
	Created int64                             `json:"created"`
	Model   string                            `json:"model"`
	Choices []ChatCompletionStreamChunkChoice `json:"choices"`
}

type ChatCompletionStreamChunkChoice struct {
	Index int                            `json:"index"`
	Delta ChatCompletionStreamChunkDelta `json:"delta"`
	// FinishReason is usually only set on the final chunk.
	FinishReason *string `json:"finish_reason,omitempty"`
}

type ChatCompletionStreamChunkDelta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
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
