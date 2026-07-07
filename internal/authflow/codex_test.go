package authflow

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveTokensMergesAuthProfiles(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "auth-profiles.json")
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
			}
		}
	}`), 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}

	key, err := saveTokens(storePath, "second@example.com", tokenResponse{
		AccessToken:  jwt(map[string]any{"email": "second@example.com", "exp": float64(time.Now().Add(time.Hour).Unix())}),
		RefreshToken: "second-refresh",
		IDToken:      jwt(map[string]any{"email": "second@example.com"}),
	})
	if err != nil {
		t.Fatalf("save tokens: %v", err)
	}
	if got, want := key, "openai-codex:second@example.com"; got != want {
		t.Fatalf("key = %q, want %q", got, want)
	}

	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var store ProfileStore
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatalf("unmarshal store: %v", err)
	}
	if _, ok := store.Profiles["openai-codex:first@example.com"]; !ok {
		t.Fatal("existing profile was removed")
	}
	second := store.Profiles["openai-codex:second@example.com"]
	if got, want := second.Refresh, "second-refresh"; got != want {
		t.Fatalf("refresh = %q, want %q", got, want)
	}
	if got, want := second.Email, "second@example.com"; got != want {
		t.Fatalf("email = %q, want %q", got, want)
	}
	if strings.TrimSpace(second.IDToken) == "" {
		t.Fatal("id token was not saved")
	}
}

func TestParseCallbackInputAcceptsFullCallbackURL(t *testing.T) {
	result, err := parseCallbackInput("http://localhost:1455/auth/callback?code=ac_123&scope=openid+profile&state=state_123")
	if err != nil {
		t.Fatalf("parse callback: %v", err)
	}
	if got, want := result.code, "ac_123"; got != want {
		t.Fatalf("code = %q, want %q", got, want)
	}
	if got, want := result.state, "state_123"; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}

func TestParseCallbackInputAcceptsQueryString(t *testing.T) {
	result, err := parseCallbackInput("?code=ac_456&state=state_456")
	if err != nil {
		t.Fatalf("parse callback: %v", err)
	}
	if got, want := result.code, "ac_456"; got != want {
		t.Fatalf("code = %q, want %q", got, want)
	}
	if got, want := result.state, "state_456"; got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}

func TestParseCallbackInputRejectsMissingState(t *testing.T) {
	if _, err := parseCallbackInput("http://localhost:1455/auth/callback?code=ac_123"); err == nil {
		t.Fatal("expected missing state error")
	}
}

func jwt(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, _ := json.Marshal(payload)
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + "."
}
