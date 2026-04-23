package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultRefreshBuffer = 5 * time.Minute

type tokenManager struct {
	mu         sync.Mutex
	httpClient *http.Client

	accessToken  string
	refreshToken string
	expiresAt    time.Time
	tokenURL     string
	clientID     string
	scopes       []string
	buffer       time.Duration
	storePath    string
}

type jwtClaims struct {
	Exp      int64  `json:"exp"`
	Issuer   string `json:"iss"`
	ClientID string `json:"client_id"`
}

type tokenErrorEnvelope struct {
	Message string
	Code    string
}

func (e *tokenErrorEnvelope) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Message = strings.TrimSpace(s)
		return nil
	}

	var obj struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Error   string `json:"error"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	e.Message = strings.TrimSpace(obj.Message)
	if e.Message == "" {
		e.Message = strings.TrimSpace(obj.Error)
	}
	e.Code = strings.TrimSpace(obj.Code)
	if e.Code == "" {
		e.Code = strings.TrimSpace(obj.Type)
	}
	return nil
}

func newTokenManager(cfg Config) *tokenManager {
	accessToken := strings.TrimSpace(cfg.AuthToken)
	tm := &tokenManager{
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		accessToken:  accessToken,
		refreshToken: strings.TrimSpace(cfg.RefreshToken),
		tokenURL:     strings.TrimSpace(cfg.TokenURL),
		clientID:     strings.TrimSpace(cfg.OAuthClientID),
		scopes:       append([]string(nil), cfg.TokenScopes...),
		buffer:       cfg.RefreshBuffer,
		storePath:    strings.TrimSpace(cfg.TokenStorePath),
	}
	if tm.buffer <= 0 {
		tm.buffer = defaultRefreshBuffer
	}
	_ = tm.loadStoreLocked()

	if !cfg.TokenExpiresAt.IsZero() {
		tm.expiresAt = cfg.TokenExpiresAt
	} else if claims, err := parseJWTClaims(accessToken); err == nil {
		if claims.Exp > 0 {
			tm.expiresAt = time.Unix(claims.Exp, 0)
		}
		if tm.tokenURL == "" && strings.TrimSpace(claims.Issuer) != "" {
			tm.tokenURL = strings.TrimRight(strings.TrimSpace(claims.Issuer), "/") + "/oauth/token"
		}
		if tm.clientID == "" {
			tm.clientID = strings.TrimSpace(claims.ClientID)
		}
	}

	return tm
}

func (tm *tokenManager) authorization(ctx context.Context, forceRefresh bool) (string, error) {
	if tm == nil {
		return "", nil
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if forceRefresh || tm.shouldRefreshLocked() {
		if err := tm.refreshLocked(ctx); err != nil {
			if forceRefresh || tm.accessToken == "" {
				return "", err
			}
		}
	}
	return normalizeBearer(tm.accessToken), nil
}

func (tm *tokenManager) canRefresh() bool {
	return tm != nil && strings.TrimSpace(tm.refreshToken) != ""
}

func (tm *tokenManager) shouldRefreshLocked() bool {
	if tm.refreshToken == "" || tm.expiresAt.IsZero() {
		return false
	}
	return time.Now().Add(tm.buffer).After(tm.expiresAt)
}

func (tm *tokenManager) refreshLocked(ctx context.Context) error {
	if tm.refreshToken == "" {
		return fmt.Errorf("refresh token is not configured")
	}
	if tm.tokenURL == "" {
		return fmt.Errorf("token URL is not configured")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", tm.refreshToken)
	if tm.clientID != "" {
		form.Set("client_id", tm.clientID)
	}
	if len(tm.scopes) > 0 {
		form.Set("scope", strings.Join(tm.scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tm.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var body struct {
		AccessToken      string             `json:"access_token"`
		RefreshToken     string             `json:"refresh_token"`
		ExpiresIn        int64              `json:"expires_in"`
		Error            tokenErrorEnvelope `json:"error"`
		ErrorDescription string             `json:"error_description"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("token refresh decode failed: %w; body=%s", err, strings.TrimSpace(string(raw)))
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if body.Error.Message != "" {
			if body.Error.Code != "" {
				return fmt.Errorf("token refresh failed: %s (%s)", body.Error.Message, body.Error.Code)
			}
			return fmt.Errorf("token refresh failed: %s", body.Error.Message)
		}
		if strings.TrimSpace(body.ErrorDescription) != "" {
			return fmt.Errorf("token refresh failed: %s", strings.TrimSpace(body.ErrorDescription))
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed != "" {
			return fmt.Errorf("token refresh failed: status %d body=%s", resp.StatusCode, trimmed)
		}
		return fmt.Errorf("token refresh failed: status %d", resp.StatusCode)
	}
	if strings.TrimSpace(body.AccessToken) == "" {
		return fmt.Errorf("token refresh failed: empty access_token")
	}

	tm.accessToken = strings.TrimSpace(body.AccessToken)
	if strings.TrimSpace(body.RefreshToken) != "" {
		tm.refreshToken = strings.TrimSpace(body.RefreshToken)
	}
	if body.ExpiresIn > 0 {
		tm.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	} else if claims, err := parseJWTClaims(tm.accessToken); err == nil && claims.Exp > 0 {
		tm.expiresAt = time.Unix(claims.Exp, 0)
	}
	return tm.saveStoreLocked()
}

func parseJWTClaims(token string) (jwtClaims, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return jwtClaims{}, fmt.Errorf("invalid JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return jwtClaims{}, err
	}
	return claims, nil
}

type tokenStore struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresAt    int64    `json:"expires_at"`
	TokenURL     string   `json:"token_url"`
	ClientID     string   `json:"client_id"`
	Scopes       []string `json:"scopes"`
}

func (tm *tokenManager) loadStoreLocked() error {
	if tm.storePath == "" {
		return nil
	}
	raw, err := os.ReadFile(tm.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var store tokenStore
	if err := json.Unmarshal(raw, &store); err != nil {
		return err
	}
	if strings.TrimSpace(store.AccessToken) != "" {
		tm.accessToken = strings.TrimSpace(store.AccessToken)
	}
	if strings.TrimSpace(store.RefreshToken) != "" {
		tm.refreshToken = strings.TrimSpace(store.RefreshToken)
	}
	if store.ExpiresAt > 0 {
		tm.expiresAt = time.UnixMilli(store.ExpiresAt)
	}
	if strings.TrimSpace(store.TokenURL) != "" {
		tm.tokenURL = strings.TrimSpace(store.TokenURL)
	}
	if strings.TrimSpace(store.ClientID) != "" {
		tm.clientID = strings.TrimSpace(store.ClientID)
	}
	if len(store.Scopes) > 0 {
		tm.scopes = append([]string(nil), store.Scopes...)
	}
	return nil
}

func (tm *tokenManager) saveStoreLocked() error {
	if tm.storePath == "" {
		return nil
	}
	store := tokenStore{
		AccessToken:  tm.accessToken,
		RefreshToken: tm.refreshToken,
		TokenURL:     tm.tokenURL,
		ClientID:     tm.clientID,
		Scopes:       append([]string(nil), tm.scopes...),
	}
	if !tm.expiresAt.IsZero() {
		store.ExpiresAt = tm.expiresAt.UnixMilli()
	}
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tm.storePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(tm.storePath, data, 0o600)
}
