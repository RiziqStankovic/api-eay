package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openclaw/customai-gateway-go/internal/cursor"
	"github.com/openclaw/customai-gateway-go/internal/types"
)

func TestBuildResponsesErrorEventIncludesLiteLLMFields(t *testing.T) {
	event := buildResponsesErrorEvent(assertErr("token refresh failed: Invalid client specified. (invalid_client)"))

	if got, want := event["type"], "error"; got != want {
		t.Fatalf("type = %v, want %v", got, want)
	}
	if got, want := event["code"], "invalid_client"; got != want {
		t.Fatalf("code = %v, want %v", got, want)
	}
	if _, ok := event["message"]; !ok {
		t.Fatal("message field missing")
	}
	if _, ok := event["param"]; !ok {
		t.Fatal("param field missing")
	}
	nested, ok := event["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field type = %T, want map[string]any", event["error"])
	}
	if got, want := nested["code"], "invalid_client"; got != want {
		t.Fatalf("nested code = %v, want %v", got, want)
	}
}

func TestBuildResponsesErrorEventMapsRefreshTokenReuseToInvalidClient(t *testing.T) {
	event := buildResponsesErrorEvent(assertErr("token refresh failed: Your refresh token has already been used to generate a new access token. Please try signing in again. (refresh_token_reused)"))

	if got, want := event["code"], "invalid_client"; got != want {
		t.Fatalf("code = %v, want %v", got, want)
	}
	nested, ok := event["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field type = %T, want map[string]any", event["error"])
	}
	if got, want := nested["code"], "invalid_client"; got != want {
		t.Fatalf("nested code = %v, want %v", got, want)
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

type fakeClient struct {
	preflightErr      error
	forceRefreshErr   error
	preflightCalls    int
	forceRefreshCalls int
	responsesEvents   []cursor.StreamEvent
}

func (f *fakeClient) ValidateModel(string) error { return nil }

func (f *fakeClient) PreflightAuth(context.Context) error {
	f.preflightCalls++
	return f.preflightErr
}

func (f *fakeClient) ForceRefreshAuth(context.Context) error {
	f.forceRefreshCalls++
	return f.forceRefreshErr
}

func (f *fakeClient) SwitchAuthProfile() bool { return false }

func (f *fakeClient) ActiveAuthProfile() string { return "" }

func (f *fakeClient) ChatCompletion(context.Context, types.ChatCompletionRequest) (types.ChatCompletionResponse, error) {
	return types.ChatCompletionResponse{}, nil
}

func (f *fakeClient) ChatCompletionStream(context.Context, types.ChatCompletionRequest, func(string) error) (types.Usage, error) {
	return types.Usage{}, nil
}

func (f *fakeClient) ResponsesStream(_ context.Context, _ types.ChatCompletionRequest, onEvent func(cursor.StreamEvent) error) (types.Usage, error) {
	for _, event := range f.responsesEvents {
		if err := onEvent(event); err != nil {
			return types.Usage{}, err
		}
	}
	return types.Usage{}, nil
}

func TestHandleResponsesStreamReturnsHTTPErrorOnPreflightFailure(t *testing.T) {
	client := &fakeClient{
		preflightErr:    assertErr("token refresh failed: Invalid client specified. (invalid_client)"),
		forceRefreshErr: assertErr("forced refresh should not be called"),
	}
	handler := NewChatCompletionsHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.3-codex",
		"input":[{"type":"message","role":"user","content":"hi"}],
		"instructions":"You are a helpful coding assistant.",
		"stream":true
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusBadGateway; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	body := rec.Body.String()
	for _, want := range []string{`"code":"invalid_client"`, `token refresh failed: Invalid client specified. (invalid_client)`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q does not contain %q", body, want)
		}
	}
	if strings.Contains(body, "response.created") {
		t.Fatalf("body %q unexpectedly contains SSE output", body)
	}
	if got, want := client.preflightCalls, 1; got != want {
		t.Fatalf("preflight calls = %d, want %d", got, want)
	}
	if got, want := client.forceRefreshCalls, 0; got != want {
		t.Fatalf("force refresh calls = %d, want %d", got, want)
	}
}

func TestHandleChatCompletionsStreamWritesToolCalls(t *testing.T) {
	index := 0
	client := &fakeClient{
		responsesEvents: []cursor.StreamEvent{
			{
				Type: "response.output_item.added",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name:      "find_path",
						Arguments: "{}",
					},
				},
			},
			{
				Type: "response.output_item.done",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name:      "find_path",
						Arguments: "{}",
					},
				},
			},
		},
	}
	handler := NewChatCompletionsHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.4-mini",
		"messages":[{"role":"user","content":"find path"}],
		"tools":[{"type":"function","function":{"name":"find_path"}}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	body := rec.Body.String()
	for _, want := range []string{`"tool_calls"`, `"name":"find_path"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q does not contain %q", body, want)
		}
	}
}

func TestHandleChatCompletionsStreamDoesNotDuplicateToolArguments(t *testing.T) {
	index := 0
	client := &fakeClient{
		responsesEvents: []cursor.StreamEvent{
			{
				Type: "response.output_item.added",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name: "find_path",
					},
				},
			},
			{
				Type: "response.function_call_arguments.delta",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Arguments: "{}",
					},
				},
			},
			{
				Type: "response.output_item.done",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name:      "find_path",
						Arguments: "{}",
					},
				},
			},
		},
	}
	handler := NewChatCompletionsHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.4-mini",
		"messages":[{"role":"user","content":"find path"}],
		"tools":[{"type":"function","function":{"name":"find_path"}}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	body := rec.Body.String()
	if got, want := strings.Count(body, `"arguments":"{}"`), 1; got != want {
		t.Fatalf("arguments chunk count = %d, want %d; body=%q", got, want, body)
	}
	if strings.Contains(body, `"arguments":"{}{}"`) {
		t.Fatalf("body contains duplicated tool arguments: %q", body)
	}
}

func TestHandleChatCompletionsStreamDoesNotDuplicateArgumentsWhenItemIDDiffers(t *testing.T) {
	index := 0
	args := `{"path":"D:\\xl\\auto-repo-xl"}`
	client := &fakeClient{
		responsesEvents: []cursor.StreamEvent{
			{
				Type: "response.output_item.added",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name: "list_directory",
					},
				},
			},
			{
				Type: "response.function_call_arguments.delta",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "fc_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Arguments: args,
					},
				},
			},
			{
				Type: "response.output_item.done",
				ToolCall: &types.ToolCall{
					Index: &index,
					ID:    "call_123",
					Type:  "function",
					Function: types.ToolCallFunction{
						Name:      "list_directory",
						Arguments: args,
					},
				},
			},
		},
	}
	handler := NewChatCompletionsHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.4-mini",
		"messages":[{"role":"user","content":"list files"}],
		"tools":[{"type":"function","function":{"name":"list_directory"}}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	body := rec.Body.String()
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if got, want := strings.Count(body, `"arguments":`+string(encodedArgs)), 1; got != want {
		t.Fatalf("arguments chunk count = %d, want %d; body=%q", got, want, body)
	}
}

func TestHandleChatCompletionsStreamWritesReasoningContent(t *testing.T) {
	client := &fakeClient{
		responsesEvents: []cursor.StreamEvent{
			{
				Type:           "response.reasoning_summary_text.delta",
				ReasoningDelta: "Checking the repo first.",
			},
		},
	}
	handler := NewChatCompletionsHandler(client)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5.4-mini",
		"messages":[{"role":"user","content":"cek"}],
		"stream":true
	}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	body := rec.Body.String()
	for _, want := range []string{`"reasoning_content":"Checking the repo first."`, `"finish_reason":"stop"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %q does not contain %q", body, want)
		}
	}
}

func TestNormalizeToolArgumentsCleansDiagnosticsNullPath(t *testing.T) {
	cases := []string{
		`null`,
		`{"path":null}`,
		`{"path":"null"}`,
		`{"path":".null"}`,
		`{"path":"/"}`,
		`{"path":"auto-repo-xl"}`,
		`{"path":"D:\\xl\\auto-repo-xl"}`,
		`{"path":"D:\\xl\\auto-repo-xl\\shared-lib-apps"}`,
		`{"directory":"D:\\xl\\auto-repo-xl"}`,
	}

	for _, raw := range cases {
		if got, want := normalizeToolArguments("diagnostics", raw), `{}`; got != want {
			t.Fatalf("normalize diagnostics %s = %s, want %s", raw, got, want)
		}
	}
}

func TestNormalizeToolArgumentsKeepsValidFilePath(t *testing.T) {
	got := normalizeToolArguments("diagnostics", `{"path":"D:\\xl\\auto-repo-xl\\package.json"}`)
	if !strings.Contains(got, `D:\\xl\\auto-repo-xl\\package.json`) {
		t.Fatalf("normalized args = %s, want path preserved", got)
	}
}

func TestNormalizeToolArgumentsMakesFindPathGlobWorkspaceRelative(t *testing.T) {
	got := normalizeToolArguments("find_path", `{"glob":"D:\\xl\\auto-repo-xl/**/README*","offset":0}`)
	for _, want := range []string{`"glob":"auto-repo-xl/**/README*"`, `"offset":0`} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized args = %s, want %s", got, want)
		}
	}
}

func TestNormalizeFindPathGlobKeepsRelativeGlob(t *testing.T) {
	if got, want := normalizeFindPathGlob(`auto-repo-xl/**/README*`), `auto-repo-xl/**/README*`; got != want {
		t.Fatalf("normalizeFindPathGlob = %q, want %q", got, want)
	}
}
