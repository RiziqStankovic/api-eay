package cursor

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/openclaw/customai-gateway-go/internal/types"
)

func TestBuildUpstreamRequestPreservesImageParts(t *testing.T) {
	req := types.ChatCompletionRequest{
		Model: "gpt-5.3-codex",
		Messages: []types.ChatCompletionMessage{
			{
				Role: "user",
				Content: types.MessageContent{
					Text: "ini gambar apa?",
					Parts: []types.MessageContentPart{
						{Type: "input_text", Text: "ini gambar apa?"},
						{Type: "input_image", ImageURL: "data:image/png;base64,abc123"},
					},
				},
			},
		},
	}

	upstream := buildUpstreamRequest(req, true, "You are helpful.")
	payload, err := json.Marshal(upstream)
	if err != nil {
		t.Fatalf("marshal upstream request: %v", err)
	}

	got := string(payload)
	for _, want := range []string{
		`"content":[`,
		`"type":"input_text"`,
		`"type":"input_image"`,
		`"image_url":"data:image/png;base64,abc123"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("upstream payload %s does not contain %s", got, want)
		}
	}
}

func TestRedactPayloadForLogRemovesImageData(t *testing.T) {
	payload := []byte(`{
		"input": [
			{
				"type": "message",
				"content": [
					{"type": "input_text", "text": "ini apa?"},
					{"type": "input_image", "image_url": "data:image/png;base64,abc123"}
				]
			}
		],
		"file_data": "base64-file"
	}`)

	got := redactPayloadForLog(payload)
	for _, forbidden := range []string{"data:image/png;base64,abc123", "base64-file"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("redacted payload still contains %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"<redacted image_url>", "<redacted file_data>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("redacted payload %s does not contain %s", got, want)
		}
	}
}

func TestParseStreamDataMarksRetryableErrorsTransient(t *testing.T) {
	data := `{
		"type": "response.failed",
		"response": {
			"error": {
				"message": "An error occurred while processing your request. You can retry your request."
			}
		}
	}`

	_, _, _, err := parseStreamData(data)
	if err == nil {
		t.Fatal("expected error")
	}
	var transient transientUpstreamError
	if !errors.As(err, &transient) {
		t.Fatalf("expected transientUpstreamError, got %T: %v", err, err)
	}
}

func TestUpstreamRequestIDReadsKnownHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Openai-Request-Id", "req_123")

	if got := upstreamRequestID(headers); got != "req_123" {
		t.Fatalf("upstreamRequestID() = %q, want req_123", got)
	}
}
