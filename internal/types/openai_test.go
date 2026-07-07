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
