package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageContentChatImageURLMarshalsAsResponsesInputImage(t *testing.T) {
	var content MessageContent
	raw := []byte(`[
		{"type":"text","text":"ini gambar apa?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,abc123","detail":"low"}}
	]`)

	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if got := content.PlainText(); got != "ini gambar apa?" {
		t.Fatalf("PlainText() = %q", got)
	}
	if !content.HasImage() {
		t.Fatal("expected image part")
	}

	payload, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	got := string(payload)
	for _, want := range []string{
		`"type":"input_text"`,
		`"type":"input_image"`,
		`"image_url":"data:image/png;base64,abc123"`,
		`"detail":"low"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("marshaled content %s does not contain %s", got, want)
		}
	}
}

func TestMessageContentTextOnlyMarshalsAsString(t *testing.T) {
	var content MessageContent
	if err := json.Unmarshal([]byte(`[{"type":"input_text","text":"hello"}]`), &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}

	payload, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	if got, want := string(payload), `"hello"`; got != want {
		t.Fatalf("marshaled content = %s, want %s", got, want)
	}
}

func TestChatCompletionMessageAllowsNullContentWithToolCalls(t *testing.T) {
	raw := []byte(`{
		"role": "assistant",
		"content": null,
		"tool_calls": [
			{
				"id": "call_123",
				"type": "function",
				"function": {
					"name": "find_path"
				}
			}
		]
	}`)

	var msg ChatCompletionMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if got, want := len(msg.ToolCalls), 1; got != want {
		t.Fatalf("tool calls len = %d, want %d", got, want)
	}
	if got, want := msg.ToolCalls[0].Function.Name, "find_path"; got != want {
		t.Fatalf("tool name = %q, want %q", got, want)
	}
}

func TestAltChatRequestPreservesResponsesToolState(t *testing.T) {
	raw := []byte(`{
		"model": "gpt-5.4-mini",
		"instructions": "You are a coding agent.",
		"input": [
			{"type":"message","role":"user","content":"check this repo"},
			{
				"type":"function_call",
				"id":"fc_123",
				"call_id":"call_123",
				"name":"list_directory",
				"arguments":"{\"path\":\"D:\\\\project\\\\miawai\"}"
			},
			{
				"type":"function_call_output",
				"call_id":"call_123",
				"output":"README.md\ncmd\ninternal"
			}
		],
		"stream": true
	}`)

	var req AltChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal alt request: %v", err)
	}

	chatReq := req.ToChatCompletionRequest()
	if got, want := len(chatReq.Messages), 4; got != want {
		t.Fatalf("messages len = %d, want %d: %#v", got, want, chatReq.Messages)
	}
	if got, want := chatReq.Messages[0].Role, "system"; got != want {
		t.Fatalf("message[0].role = %q, want %q", got, want)
	}
	assistant := chatReq.Messages[2]
	if got, want := assistant.Role, "assistant"; got != want {
		t.Fatalf("assistant role = %q, want %q", got, want)
	}
	if got, want := len(assistant.ToolCalls), 1; got != want {
		t.Fatalf("tool calls len = %d, want %d", got, want)
	}
	if got, want := assistant.ToolCalls[0].ID, "call_123"; got != want {
		t.Fatalf("tool call id = %q, want %q", got, want)
	}
	if got, want := assistant.ToolCalls[0].Function.Name, "list_directory"; got != want {
		t.Fatalf("tool call name = %q, want %q", got, want)
	}
	tool := chatReq.Messages[3]
	if got, want := tool.Role, "tool"; got != want {
		t.Fatalf("tool role = %q, want %q", got, want)
	}
	if got, want := tool.ToolCallID, "call_123"; got != want {
		t.Fatalf("tool call id = %q, want %q", got, want)
	}
	if !strings.Contains(tool.Content.PlainText(), "README.md") {
		t.Fatalf("tool output = %q, want README.md", tool.Content.PlainText())
	}
}
