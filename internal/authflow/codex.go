package authflow

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	CodexOAuthIssuer       = "https://auth.openai.com"
	DefaultCodexClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexScope      = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	DefaultCodexOriginator = "codex_cli_rs"
	DefaultCallbackPort    = 1455
)

type Options struct {
	StorePath     string
	Profile       string
	ClientID      string
	Scope         string
	Originator    string
	CallbackPort  int
	OpenBrowser   bool
	Timeout       time.Duration
	PasteCallback bool
	CallbackInput io.Reader
}

type ProfileStore struct {
	Version  int                `json:"version"`
	Profiles map[string]Profile `json:"profiles"`
}

type Profile struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Access   string `json:"access"`
	Refresh  string `json:"refresh"`
	IDToken  string `json:"id_token,omitempty"`
	Expires  int64  `json:"expires"`
	Email    string `json:"email,omitempty"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type callbackResult struct {
	code  string
	state string
	err   string
}

func Login(ctx context.Context, opts Options) (string, error) {
	opts = normalizeOptions(opts)

	verifier, err := randomURLSafe(64)
	if err != nil {
		return "", err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}

	listener, actualPort, err := listenCallback(opts.CallbackPort)
	if err != nil {
		return "", err
	}
	defer listener.Close()

	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", actualPort)
	authURL := buildAuthorizeURL(opts, redirectURI, verifier, state)

	resultCh := make(chan callbackResult, 1)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/auth/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			result := callbackResult{
				code:  strings.TrimSpace(q.Get("code")),
				state: strings.TrimSpace(q.Get("state")),
				err:   strings.TrimSpace(q.Get("error")),
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if result.err != "" || result.code == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("<h1>Codex login failed</h1><p>You can close this tab.</p>"))
			} else {
				_, _ = w.Write([]byte("<h1>Codex login complete</h1><p>You can return to the terminal.</p>"))
			}
			select {
			case resultCh <- result:
			default:
			}
		}),
	}
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Shutdown(context.Background())

	fmt.Printf("Open this URL to login:\n\n%s\n\n", authURL)
	if opts.PasteCallback {
		fmt.Printf("If the browser callback does not trigger, paste the full callback URL here and press Enter.\n\n")
		go readPastedCallback(opts.CallbackInput, resultCh)
	}
	if opts.OpenBrowser {
		_ = openBrowser(authURL)
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var result callbackResult
	select {
	case result = <-resultCh:
	case <-waitCtx.Done():
		return "", fmt.Errorf("login timed out waiting for callback")
	}
	if result.err != "" {
		return "", fmt.Errorf("oauth error: %s", result.err)
	}
	if result.state != state {
		return "", fmt.Errorf("oauth state mismatch")
	}
	if result.code == "" {
		return "", fmt.Errorf("oauth callback did not include code")
	}

	tokens, err := exchangeCode(waitCtx, opts, redirectURI, verifier, result.code)
	if err != nil {
		return "", err
	}
	key, err := saveTokens(opts.StorePath, opts.Profile, tokens)
	if err != nil {
		return "", err
	}
	return key, nil
}

func normalizeOptions(opts Options) Options {
	if strings.TrimSpace(opts.StorePath) == "" {
		opts.StorePath = "auth-profiles.json"
	}
	if strings.TrimSpace(opts.ClientID) == "" {
		opts.ClientID = DefaultCodexClientID
	}
	if strings.TrimSpace(opts.Scope) == "" {
		opts.Scope = DefaultCodexScope
	}
	if strings.TrimSpace(opts.Originator) == "" {
		opts.Originator = DefaultCodexOriginator
	}
	if opts.CallbackPort <= 0 {
		opts.CallbackPort = DefaultCallbackPort
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.CallbackInput == nil {
		opts.CallbackInput = os.Stdin
	}
	return opts
}

func readPastedCallback(input io.Reader, resultCh chan<- callbackResult) {
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		result, err := parseCallbackInput(line)
		if err != nil {
			fmt.Printf("Invalid callback URL: %v\nPaste the full callback URL again, or wait for browser callback.\n\n", err)
			continue
		}
		select {
		case resultCh <- result:
		default:
		}
		return
	}
}

func parseCallbackInput(input string) (callbackResult, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return callbackResult{}, fmt.Errorf("empty callback input")
	}

	var values url.Values
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		u, err := url.Parse(input)
		if err != nil {
			return callbackResult{}, err
		}
		if !strings.HasSuffix(u.Path, "/auth/callback") {
			return callbackResult{}, fmt.Errorf("URL path %q is not /auth/callback", u.Path)
		}
		values = u.Query()
	} else {
		input = strings.TrimPrefix(input, "?")
		parsed, err := url.ParseQuery(input)
		if err != nil {
			return callbackResult{}, err
		}
		values = parsed
	}

	result := callbackResult{
		code:  strings.TrimSpace(values.Get("code")),
		state: strings.TrimSpace(values.Get("state")),
		err:   strings.TrimSpace(values.Get("error")),
	}
	if result.err == "" && result.code == "" {
		return callbackResult{}, fmt.Errorf("missing code")
	}
	if result.state == "" {
		return callbackResult{}, fmt.Errorf("missing state")
	}
	return result, nil
}

func listenCallback(preferredPort int) (net.Listener, int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", preferredPort)
	listener, err := net.Listen("tcp", addr)
	if err == nil {
		return listener, preferredPort, nil
	}
	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return listener, listener.Addr().(*net.TCPAddr).Port, nil
}

func buildAuthorizeURL(opts Options, redirectURI, verifier, state string) string {
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	u, _ := url.Parse(CodexOAuthIssuer + "/oauth/authorize")
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", opts.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", opts.Scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", opts.Originator)
	u.RawQuery = q.Encode()
	return u.String()
}

func exchangeCode(ctx context.Context, opts Options, redirectURI, verifier, code string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", opts.ClientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, CodexOAuthIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenResponse{}, fmt.Errorf("token exchange failed: status %d body=%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var tokens tokenResponse
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return tokenResponse{}, err
	}
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" {
		return tokenResponse{}, fmt.Errorf("token exchange did not return access and refresh tokens")
	}
	return tokens, nil
}

func saveTokens(path, selector string, tokens tokenResponse) (string, error) {
	store := ProfileStore{Version: 1, Profiles: map[string]Profile{}}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &store); err != nil {
			return "", err
		}
	}
	if store.Version == 0 {
		store.Version = 1
	}
	if store.Profiles == nil {
		store.Profiles = map[string]Profile{}
	}

	email := firstNonEmpty(parseJWTString(tokens.IDToken, "email"), parseJWTString(tokens.AccessToken, "email"))
	expiresAt := expiresAtMillis(tokens)
	key := resolveProfileKey(store.Profiles, selector, email)
	if key == "" {
		key = "openai-codex:" + firstNonEmpty(email, "default")
	}

	store.Profiles[key] = Profile{
		Type:     "oauth",
		Provider: "openai-codex",
		Access:   strings.TrimSpace(tokens.AccessToken),
		Refresh:  strings.TrimSpace(tokens.RefreshToken),
		IDToken:  strings.TrimSpace(tokens.IDToken),
		Expires:  expiresAt,
		Email:    email,
	}

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return key, nil
}

func resolveProfileKey(profiles map[string]Profile, selector, email string) string {
	selector = strings.TrimSpace(selector)
	if selector != "" {
		if _, ok := profiles[selector]; ok {
			return selector
		}
		for key, profile := range profiles {
			if strings.EqualFold(profile.Email, selector) ||
				strings.EqualFold(profile.Provider+":"+profile.Email, selector) ||
				strings.EqualFold(key, selector) {
				return key
			}
		}
		if strings.Contains(selector, ":") {
			return selector
		}
		return "openai-codex:" + selector
	}
	if email != "" {
		for key, profile := range profiles {
			if strings.EqualFold(profile.Email, email) {
				return key
			}
		}
		return "openai-codex:" + email
	}
	return ""
}

func expiresAtMillis(tokens tokenResponse) int64 {
	if tokens.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UnixMilli()
	}
	if exp := parseJWTNumber(tokens.AccessToken, "exp"); exp > 0 {
		return exp * 1000
	}
	if exp := parseJWTNumber(tokens.IDToken, "exp"); exp > 0 {
		return exp * 1000
	}
	return 0
}

func parseJWTString(token, key string) string {
	payload := parseJWTPayload(token)
	if value, ok := payload[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func parseJWTNumber(token, key string) int64 {
	payload := parseJWTPayload(token)
	switch value := payload[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	}
	return 0
}

func parseJWTPayload(token string) map[string]any {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func openBrowser(rawURL string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	case "darwin":
		return exec.Command("open", rawURL).Start()
	default:
		return exec.Command("xdg-open", rawURL).Start()
	}
}
