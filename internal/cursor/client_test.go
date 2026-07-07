package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBuildUpstreamRequestOmitsOutputOnMessageInput(t *testing.T) {
	req := types.ChatCompletionRequest{
		Model: "gpt-5.4-mini",
		Messages: []types.ChatCompletionMessage{
			{Role: "user", Content: types.MessageContent{Text: "check this repo"}},
		},
	}

	upstream := buildUpstreamRequest(req, true, "You are helpful.")
	payload, err := json.Marshal(upstream)
	if err != nil {
		t.Fatalf("marshal upstream request: %v", err)
	}

	got := string(payload)
	if strings.Contains(got, `"output"`) {
		t.Fatalf("upstream payload unexpectedly contains output field: %s", got)
	}
}

func TestBuildUpstreamRequestAddsZedToolGuidance(t *testing.T) {
	req := types.ChatCompletionRequest{
		Model: "gpt-5.4-mini",
		Messages: []types.ChatCompletionMessage{
			{Role: "user", Content: types.MessageContent{Text: "cek project"}},
		},
		Tools: []types.ToolDefinition{
			{
				Type: "function",
				Function: types.ToolFunction{
					Name: "find_path",
				},
			},
		},
	}

	upstream := buildUpstreamRequest(req, true, "You are helpful.")
	if !strings.Contains(upstream.Instructions, "Do not repeat the same broad pattern after a \"No matches\" result") {
		t.Fatalf("instructions missing Zed tool guidance: %s", upstream.Instructions)
	}
}

func TestBuildUpstreamRequestMapsReasoningEffort(t *testing.T) {
	req := types.ChatCompletionRequest{
		Model: "gpt-5.4-mini",
		Messages: []types.ChatCompletionMessage{
			{Role: "user", Content: types.MessageContent{Text: "cek project"}},
		},
		ReasoningEffort: "Extra High",
	}

	upstream := buildUpstreamRequest(req, true, "You are helpful.")
	reasoning, ok := upstream.Reasoning.(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %T, want map[string]any", upstream.Reasoning)
	}
	if got, want := reasoning["effort"], "high"; got != want {
		t.Fatalf("reasoning effort = %v, want %v", got, want)
	}
}

func TestBuildUpstreamRequestKeepsReasoningObject(t *testing.T) {
	req := types.ChatCompletionRequest{
		Model: "gpt-5.4-mini",
		Messages: []types.ChatCompletionMessage{
			{Role: "user", Content: types.MessageContent{Text: "cek project"}},
		},
		Reasoning: map[string]any{
			"effort":  "Medium",
			"summary": "auto",
		},
	}

	upstream := buildUpstreamRequest(req, true, "You are helpful.")
	reasoning, ok := upstream.Reasoning.(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %T, want map[string]any", upstream.Reasoning)
	}
	if got, want := reasoning["effort"], "medium"; got != want {
		t.Fatalf("reasoning effort = %v, want %v", got, want)
	}
	if got, want := reasoning["summary"], "auto"; got != want {
		t.Fatalf("reasoning summary = %v, want %v", got, want)
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

	_, _, err := parseStreamData(data)
	if err == nil {
		t.Fatal("expected error")
	}
	var transient transientUpstreamError
	if !errors.As(err, &transient) {
		t.Fatalf("expected transientUpstreamError, got %T: %v", err, err)
	}
}

func TestParseStreamDataReadsFunctionCallItem(t *testing.T) {
	data := `{
		"type": "response.output_item.added",
		"output_index": 0,
		"item": {
			"id": "fc_123",
			"call_id": "call_123",
			"type": "function_call",
			"name": "find_path"
		}
	}`

	event, _, err := parseStreamData(data)
	if err != nil {
		t.Fatalf("parseStreamData() error = %v", err)
	}
	if event.ToolCall == nil {
		t.Fatal("ToolCall is nil")
	}
	if got, want := event.ToolCall.Function.Name, "find_path"; got != want {
		t.Fatalf("tool name = %q, want %q", got, want)
	}
	if got, want := event.ToolCall.Function.Arguments, ""; got != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	if event.ToolCall.Index == nil || *event.ToolCall.Index != 0 {
		t.Fatalf("index = %v, want 0", event.ToolCall.Index)
	}
}

func TestParseStreamDataReadsReasoningSummaryDelta(t *testing.T) {
	data := `{
		"type": "response.reasoning_summary_text.delta",
		"delta": "I need to inspect the repo."
	}`

	event, _, err := parseStreamData(data)
	if err != nil {
		t.Fatalf("parseStreamData() error = %v", err)
	}
	if got, want := event.ReasoningDelta, "I need to inspect the repo."; got != want {
		t.Fatalf("ReasoningDelta = %q, want %q", got, want)
	}
}

func TestUpstreamRequestIDReadsKnownHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("Openai-Request-Id", "req_123")

	if got := upstreamRequestID(headers); got != "req_123" {
		t.Fatalf("upstreamRequestID() = %q, want req_123", got)
	}
}

func TestAuthProfileFailureIncludesRefreshTokenReuse(t *testing.T) {
	err := errors.New("token refresh failed: Your refresh token has already been used. (refresh_token_reused)")

	if !isAuthProfileFailure(err) {
		t.Fatal("isAuthProfileFailure() = false, want true")
	}
}

func TestChatGPTAccountIDFromTokenReadsNestedClaim(t *testing.T) {
	token := "Bearer " + testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
		},
	})

	if got, want := chatGPTAccountIDFromToken(token), "acct-123"; got != want {
		t.Fatalf("account id = %q, want %q", got, want)
	}
}

func TestNewTokenManagerPrefersLaterJWTExpiry(t *testing.T) {
	cfg := Config{
		AuthToken:      "Bearer eyJhbGciOiJub25lIn0.eyJleHAiOjIwMDAwMDAwMDAsImlzcyI6Imh0dHBzOi8vYXV0aC5leGFtcGxlLmNvbSIsImNsaWVudF9pZCI6ImNsaWVudC0xMjMifQ.",
		TokenExpiresAt: time.Unix(1000, 0),
	}

	tm := newTokenManager(cfg)

	if got, want := tm.expiresAt.Unix(), int64(2000000000); got != want {
		t.Fatalf("expiresAt unix = %d, want %d", got, want)
	}
	if got, want := tm.tokenURL, "https://auth.example.com/oauth/token"; got != want {
		t.Fatalf("tokenURL = %q, want %q", got, want)
	}
	if got, want := tm.clientID, "client-123"; got != want {
		t.Fatalf("clientID = %q, want %q", got, want)
	}
}

func testJWT(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, _ := json.Marshal(payload)
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + "."
}

func TestNewTokenManagerIgnoresTokenStoreWithoutRefreshToken(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(storePath, []byte(`{
		"access_token":"Bearer stored-access",
		"refresh_token":"stored-refresh",
		"expires_at":1234567890,
		"token_url":"https://auth.example.com/oauth/token",
		"client_id":"client-123"
	}`), 0o600); err != nil {
		t.Fatalf("write token store: %v", err)
	}

	tm := newTokenManager(Config{
		AuthToken:      "Bearer live-access",
		TokenStorePath: storePath,
	})

	if got, want := tm.accessToken, "Bearer live-access"; got != want {
		t.Fatalf("accessToken = %q, want %q", got, want)
	}
	if got, want := tm.refreshToken, ""; got != want {
		t.Fatalf("refreshToken = %q, want %q", got, want)
	}
	if got, want := tm.storePath, ""; got != want {
		t.Fatalf("storePath = %q, want %q", got, want)
	}
}

func TestNewTokenManagerLoadsLegacyProfileStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:test@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "stored-access",
				"refresh": "stored-refresh",
				"expires": 1778380021542,
				"email": "test@example.com"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		AuthToken:      "live-access",
		RefreshToken:   "live-refresh",
		TokenStorePath: storePath,
	})

	if got, want := tm.accessToken, "stored-access"; got != want {
		t.Fatalf("accessToken = %q, want %q", got, want)
	}
	if got, want := tm.refreshToken, "stored-refresh"; got != want {
		t.Fatalf("refreshToken = %q, want %q", got, want)
	}
	if got, want := tm.expiresAt.UnixMilli(), int64(1778380021542); got != want {
		t.Fatalf("expiresAt = %d, want %d", got, want)
	}
}

func TestNewTokenManagerSelectsLegacyProfileByEmail(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:first@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "first-access",
				"refresh": "first-refresh",
				"expires": 1778380021542,
				"email": "first@example.com"
			},
			"openai-codex:second@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "second-access",
				"refresh": "second-refresh",
				"expires": 1779553725680,
				"email": "second@example.com"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		RefreshToken:   "env-refresh",
		TokenStorePath: storePath,
		TokenProfile:   "second@example.com",
	})

	if got, want := tm.accessToken, "second-access"; got != want {
		t.Fatalf("accessToken = %q, want %q", got, want)
	}
	if got, want := tm.refreshToken, "second-refresh"; got != want {
		t.Fatalf("refreshToken = %q, want %q", got, want)
	}
	if got, want := tm.expiresAt.UnixMilli(), int64(1779553725680); got != want {
		t.Fatalf("expiresAt = %d, want %d", got, want)
	}
}

func TestSelectLegacyProfileRequiresSelectorForMultipleProfiles(t *testing.T) {
	_, _, err := selectLegacyProfile(map[string]legacyProfile{
		"openai-codex:first@example.com": {
			Access: "first-access",
			Email:  "first@example.com",
		},
		"openai-codex:second@example.com": {
			Access: "second-access",
			Email:  "second@example.com",
		},
	}, "")

	if err == nil {
		t.Fatal("expected multiple profile error")
	}
	if !strings.Contains(err.Error(), "CUSTOMAI_TOKEN_PROFILE") {
		t.Fatalf("error = %q, want CUSTOMAI_TOKEN_PROFILE hint", err.Error())
	}
}

func TestNewTokenManagerLoadsAllLegacyProfilesForFallback(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:b@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "b-access",
				"refresh": "b-refresh",
				"expires": 222,
				"email": "b@example.com"
			},
			"openai-codex:a@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "a-access",
				"refresh": "a-refresh",
				"expires": 111,
				"email": "a@example.com"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		RefreshToken:   "env-refresh",
		TokenStorePath: storePath,
		TokenProfile:   "*",
	})

	if got, want := tm.accessToken, "a-access"; got != want {
		t.Fatalf("accessToken = %q, want %q", got, want)
	}
	if got, want := tm.profileKey, "openai-codex:a@example.com"; got != want {
		t.Fatalf("profileKey = %q, want %q", got, want)
	}
	if got, want := len(tm.profiles), 2; got != want {
		t.Fatalf("profiles len = %d, want %d", got, want)
	}
	if !tm.switchToNextProfile() {
		t.Fatal("switchToNextProfile() = false, want true")
	}
	if got, want := tm.accessToken, "b-access"; got != want {
		t.Fatalf("accessToken after switch = %q, want %q", got, want)
	}
	if got, want := tm.profileKey, "openai-codex:b@example.com"; got != want {
		t.Fatalf("profileKey after switch = %q, want %q", got, want)
	}
	if tm.switchToNextProfile() {
		t.Fatal("switchToNextProfile() = true at end, want false")
	}
}

func TestSaveStoreUpdatesSelectedLegacyProfile(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:first@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "first-access",
				"refresh": "first-refresh",
				"expires": 111,
				"email": "first@example.com"
			},
			"openai-codex:second@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "second-access",
				"refresh": "second-refresh",
				"expires": 222,
				"email": "second@example.com"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		RefreshToken:   "env-refresh",
		TokenStorePath: storePath,
		TokenProfile:   "second@example.com",
	})
	tm.accessToken = "second-access-new"
	tm.refreshToken = "second-refresh-new"
	tm.expiresAt = time.UnixMilli(333)

	if err := tm.saveStoreLocked(); err != nil {
		t.Fatalf("save store: %v", err)
	}

	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var legacy legacyTokenStore
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy store: %v", err)
	}

	first := legacy.Profiles["openai-codex:first@example.com"]
	if got, want := first.Access, "first-access"; got != want {
		t.Fatalf("first access = %q, want %q", got, want)
	}
	if got, want := first.Refresh, "first-refresh"; got != want {
		t.Fatalf("first refresh = %q, want %q", got, want)
	}

	second := legacy.Profiles["openai-codex:second@example.com"]
	if got, want := second.Access, "second-access-new"; got != want {
		t.Fatalf("second access = %q, want %q", got, want)
	}
	if got, want := second.Refresh, "second-refresh-new"; got != want {
		t.Fatalf("second refresh = %q, want %q", got, want)
	}
	if got, want := second.Expires, int64(333); got != want {
		t.Fatalf("second expires = %d, want %d", got, want)
	}
}

func TestMarkRefreshFailureDisablesTerminalLegacyProfile(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:first@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "first-access",
				"refresh": "first-refresh",
				"expires": 111,
				"email": "first@example.com"
			},
			"openai-codex:second@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "second-access",
				"refresh": "second-refresh",
				"expires": 222,
				"email": "second@example.com"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		RefreshToken:   "env-refresh",
		TokenStorePath: storePath,
		TokenProfile:   "first@example.com,second@example.com",
	})
	if err := tm.markRefreshFailureLocked(errors.New("token refresh failed: reused (refresh_token_reused)")); err != nil {
		t.Fatalf("mark refresh failure: %v", err)
	}

	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var legacy legacyTokenStore
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy store: %v", err)
	}
	first := legacy.Profiles["openai-codex:first@example.com"]
	if !first.Disabled {
		t.Fatal("first profile disabled = false, want true")
	}
	if first.LastRefreshFailureAt == 0 {
		t.Fatal("first profile last refresh failure was not set")
	}
	if !tm.switchToNextProfile() {
		t.Fatal("switchToNextProfile() = false, want true")
	}
	if got, want := tm.profileKey, "openai-codex:second@example.com"; got != want {
		t.Fatalf("profileKey = %q, want %q", got, want)
	}
}

func TestSaveLegacyStoreClearsRefreshFailure(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:first@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "first-access",
				"refresh": "first-refresh",
				"expires": 111,
				"email": "first@example.com",
				"disabled": true,
				"last_refresh_failure_at": 222,
				"last_refresh_error": "old error"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		RefreshToken:   "env-refresh",
		TokenStorePath: storePath,
		TokenProfile:   "first@example.com",
	})
	tm.accessToken = "first-access-new"
	tm.refreshToken = "first-refresh-new"
	if err := tm.saveStoreLocked(); err != nil {
		t.Fatalf("save store: %v", err)
	}

	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var legacy legacyTokenStore
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("unmarshal legacy store: %v", err)
	}
	first := legacy.Profiles["openai-codex:first@example.com"]
	if first.Disabled {
		t.Fatal("first profile disabled = true, want false")
	}
	if first.LastRefreshFailureAt != 0 || first.LastRefreshError != "" {
		t.Fatalf("failure state = (%d, %q), want cleared", first.LastRefreshFailureAt, first.LastRefreshError)
	}
}

func TestForceRefreshIgnoresFutureExpiryAfterReload(t *testing.T) {
	refreshCalls := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(storePath, []byte(`{
		"version": 1,
		"profiles": {
			"openai-codex:first@example.com": {
				"type": "oauth",
				"provider": "openai-codex",
				"access": "old-access",
				"refresh": "old-refresh",
				"expires": 4102444800000,
				"email": "first@example.com"
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write legacy token store: %v", err)
	}

	tm := newTokenManager(Config{
		RefreshToken:   "env-refresh",
		TokenStorePath: storePath,
		TokenProfile:   "first@example.com",
		TokenURL:       tokenServer.URL,
	})

	token, err := tm.authorization(context.Background(), true)
	if err != nil {
		t.Fatalf("authorization force refresh: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if got, want := token, "Bearer new-access"; got != want {
		t.Fatalf("token = %q, want %q", got, want)
	}
}
