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
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultRefreshBuffer = 5 * time.Minute
const refreshFailureCooldown = 60 * time.Second

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
	profile      string
	storeLegacy  bool
	profileKey   string
	profiles     []legacyProfileEntry
	activeIndex  int
	initErr      error
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
	refreshToken := strings.TrimSpace(cfg.RefreshToken)
	storePath := strings.TrimSpace(cfg.TokenStorePath)
	if refreshToken == "" && accessToken != "" {
		storePath = ""
	}
	tm := &tokenManager{
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		accessToken:  accessToken,
		refreshToken: refreshToken,
		tokenURL:     strings.TrimSpace(cfg.TokenURL),
		clientID:     strings.TrimSpace(cfg.OAuthClientID),
		scopes:       append([]string(nil), cfg.TokenScopes...),
		buffer:       cfg.RefreshBuffer,
		storePath:    storePath,
		profile:      strings.TrimSpace(cfg.TokenProfile),
	}
	if tm.buffer <= 0 {
		tm.buffer = defaultRefreshBuffer
	}
	tm.initErr = tm.loadStoreLocked()

	if !cfg.TokenExpiresAt.IsZero() {
		tm.expiresAt = cfg.TokenExpiresAt
	}
	if claims, err := parseJWTClaims(accessToken); err == nil {
		if claims.Exp > 0 {
			tm.expiresAt = laterTime(tm.expiresAt, time.Unix(claims.Exp, 0))
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

func laterTime(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() {
		return a
	}
	if b.After(a) {
		return b
	}
	return a
}

func (tm *tokenManager) authorization(ctx context.Context, forceRefresh bool) (string, error) {
	if tm == nil {
		return "", nil
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.initErr != nil {
		return "", tm.initErr
	}

	if forceRefresh || tm.shouldRefreshLocked() {
		if err := tm.refreshLocked(ctx, forceRefresh); err != nil {
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

func (tm *tokenManager) refreshLocked(ctx context.Context, forceRefresh bool) error {
	if tm.storePath != "" {
		unlock, err := acquireFileLock(ctx, tm.storePath+".lock", 15*time.Second)
		if err != nil {
			return err
		}
		defer unlock()
		if tm.storeLegacy {
			previousAccess := tm.accessToken
			if err := tm.reloadActiveLegacyProfileLocked(); err != nil {
				return err
			}
			if strings.TrimSpace(previousAccess) != "" && tm.accessToken != previousAccess {
				return nil
			}
			if !forceRefresh && !tm.shouldRefreshLocked() {
				return nil
			}
		}
	}
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
		refreshErr := tokenRefreshError(resp.StatusCode, raw, body.Error, body.ErrorDescription)
		_ = tm.markRefreshFailureLocked(refreshErr)
		return refreshErr
	}
	if strings.TrimSpace(body.AccessToken) == "" {
		refreshErr := fmt.Errorf("token refresh failed: empty access_token")
		_ = tm.markRefreshFailureLocked(refreshErr)
		return refreshErr
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

func tokenRefreshError(status int, raw []byte, bodyErr tokenErrorEnvelope, errorDescription string) error {
	if bodyErr.Message != "" {
		if bodyErr.Code != "" {
			return fmt.Errorf("token refresh failed: %s (%s)", bodyErr.Message, bodyErr.Code)
		}
		return fmt.Errorf("token refresh failed: %s", bodyErr.Message)
	}
	if strings.TrimSpace(errorDescription) != "" {
		return fmt.Errorf("token refresh failed: %s", strings.TrimSpace(errorDescription))
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" {
		return fmt.Errorf("token refresh failed: status %d body=%s", status, trimmed)
	}
	return fmt.Errorf("token refresh failed: status %d", status)
}

func isTerminalRefreshError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "refresh_token_reused") ||
		strings.Contains(msg, "invalid_client") ||
		strings.Contains(msg, "invalid_grant")
}

func acquireFileLock(ctx context.Context, lockPath string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("token store lock timeout: %s", lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
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

type legacyTokenStore struct {
	Version  int                      `json:"version"`
	Profiles map[string]legacyProfile `json:"profiles"`
}

type legacyProfile struct {
	Type                 string `json:"type"`
	Provider             string `json:"provider"`
	Access               string `json:"access"`
	Refresh              string `json:"refresh"`
	IDToken              string `json:"id_token,omitempty"`
	Expires              int64  `json:"expires"`
	Email                string `json:"email"`
	Disabled             bool   `json:"disabled,omitempty"`
	LastRefreshFailureAt int64  `json:"last_refresh_failure_at,omitempty"`
	LastRefreshError     string `json:"last_refresh_error,omitempty"`
}

type legacyProfileEntry struct {
	Key     string
	Profile legacyProfile
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
	if strings.TrimSpace(store.AccessToken) == "" && strings.TrimSpace(store.RefreshToken) == "" {
		var legacy legacyTokenStore
		if err := json.Unmarshal(raw, &legacy); err == nil {
			entries, err := selectLegacyProfiles(legacy.Profiles, tm.profile)
			if err != nil {
				return err
			}
			if len(entries) > 0 {
				entry := entries[0]
				tm.storeLegacy = true
				tm.profileKey = entry.Key
				tm.profiles = entries
				tm.activeIndex = 0
				store.AccessToken = strings.TrimSpace(entry.Profile.Access)
				store.RefreshToken = strings.TrimSpace(entry.Profile.Refresh)
				store.ExpiresAt = entry.Profile.Expires
			}
		}
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

func selectLegacyProfile(profiles map[string]legacyProfile, selector string) (string, *legacyProfile, error) {
	entries, err := selectLegacyProfiles(profiles, selector)
	if err != nil || len(entries) == 0 {
		return "", nil, err
	}
	return entries[0].Key, &entries[0].Profile, nil
}

func selectLegacyProfiles(profiles map[string]legacyProfile, selector string) ([]legacyProfileEntry, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	selector = strings.TrimSpace(selector)
	if selector != "" {
		if selector == "*" || strings.EqualFold(selector, "all") {
			return allLegacyProfiles(profiles), nil
		}
		var entries []legacyProfileEntry
		for _, part := range strings.Split(selector, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, profile, ok := findLegacyProfile(profiles, part)
			if !ok {
				return nil, fmt.Errorf("token profile %q not found", part)
			}
			entries = append(entries, legacyProfileEntry{Key: key, Profile: profile})
		}
		if len(entries) == 0 {
			return nil, nil
		}
		return entries, nil
	}

	entries := allLegacyProfiles(profiles)
	if len(entries) > 1 {
		return nil, fmt.Errorf("multiple token profiles found; set CUSTOMAI_TOKEN_PROFILE")
	}
	return entries, nil
}

func allLegacyProfiles(profiles map[string]legacyProfile) []legacyProfileEntry {
	now := time.Now()
	keys := make([]string, 0, len(profiles))
	for key, profile := range profiles {
		if strings.TrimSpace(profile.Access) == "" {
			continue
		}
		if profile.Disabled || isProfileCoolingDown(profile, now) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]legacyProfileEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, legacyProfileEntry{Key: key, Profile: profiles[key]})
	}
	return entries
}

func isProfileCoolingDown(profile legacyProfile, now time.Time) bool {
	if profile.LastRefreshFailureAt <= 0 {
		return false
	}
	return now.Sub(time.UnixMilli(profile.LastRefreshFailureAt)) < refreshFailureCooldown
}

func findLegacyProfile(profiles map[string]legacyProfile, selector string) (string, legacyProfile, bool) {
	if profile, ok := profiles[selector]; ok {
		return selector, profile, true
	}
	for key, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(profile.Email), selector) ||
			strings.EqualFold(strings.TrimSpace(profile.Provider)+":"+strings.TrimSpace(profile.Email), selector) ||
			strings.EqualFold(key, selector) {
			return key, profile, true
		}
	}
	return "", legacyProfile{}, false
}

func (tm *tokenManager) saveStoreLocked() error {
	if tm.storePath == "" {
		return nil
	}
	if tm.storeLegacy {
		return tm.saveLegacyStoreLocked()
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

func (tm *tokenManager) switchToNextProfileLocked() bool {
	if len(tm.profiles) <= 1 || tm.activeIndex+1 >= len(tm.profiles) {
		return false
	}
	for tm.activeIndex+1 < len(tm.profiles) {
		tm.activeIndex++
		entry := tm.profiles[tm.activeIndex]
		tm.profileKey = entry.Key
		tm.accessToken = strings.TrimSpace(entry.Profile.Access)
		tm.refreshToken = strings.TrimSpace(entry.Profile.Refresh)
		if entry.Profile.Expires > 0 {
			tm.expiresAt = time.UnixMilli(entry.Profile.Expires)
		} else {
			tm.expiresAt = time.Time{}
		}
		if strings.TrimSpace(tm.accessToken) != "" && !entry.Profile.Disabled && !isProfileCoolingDown(entry.Profile, time.Now()) {
			return true
		}
	}
	return false
}

func (tm *tokenManager) switchToNextProfile() bool {
	if tm == nil {
		return false
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.switchToNextProfileLocked()
}

func (tm *tokenManager) activeProfile() string {
	if tm == nil {
		return ""
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.profileKey
}

func (tm *tokenManager) saveLegacyStoreLocked() error {
	raw, err := os.ReadFile(tm.storePath)
	if err != nil {
		return err
	}
	var legacy legacyTokenStore
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return err
	}
	key := tm.profileKey
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("legacy token profile key is not configured")
	}
	profile, ok := legacy.Profiles[key]
	if !ok {
		return fmt.Errorf("legacy token profile %q not found", key)
	}
	profile.Access = tm.accessToken
	profile.Refresh = tm.refreshToken
	profile.Disabled = false
	profile.LastRefreshFailureAt = 0
	profile.LastRefreshError = ""
	if !tm.expiresAt.IsZero() {
		profile.Expires = tm.expiresAt.UnixMilli()
	}
	legacy.Profiles[key] = profile

	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tm.storePath, data, 0o600)
}

func (tm *tokenManager) reloadActiveLegacyProfileLocked() error {
	if !tm.storeLegacy || strings.TrimSpace(tm.profileKey) == "" {
		return nil
	}
	raw, err := os.ReadFile(tm.storePath)
	if err != nil {
		return err
	}
	var legacy legacyTokenStore
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return err
	}
	profile, ok := legacy.Profiles[tm.profileKey]
	if !ok {
		return fmt.Errorf("legacy token profile %q not found", tm.profileKey)
	}
	tm.accessToken = strings.TrimSpace(profile.Access)
	tm.refreshToken = strings.TrimSpace(profile.Refresh)
	if profile.Expires > 0 {
		tm.expiresAt = time.UnixMilli(profile.Expires)
	} else {
		tm.expiresAt = time.Time{}
	}
	for i := range tm.profiles {
		if tm.profiles[i].Key == tm.profileKey {
			tm.profiles[i].Profile = profile
			break
		}
	}
	return nil
}

func (tm *tokenManager) markRefreshFailureLocked(refreshErr error) error {
	if !tm.storeLegacy || strings.TrimSpace(tm.profileKey) == "" || tm.storePath == "" {
		return nil
	}
	raw, err := os.ReadFile(tm.storePath)
	if err != nil {
		return err
	}
	var legacy legacyTokenStore
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return err
	}
	profile, ok := legacy.Profiles[tm.profileKey]
	if !ok {
		return fmt.Errorf("legacy token profile %q not found", tm.profileKey)
	}
	profile.LastRefreshFailureAt = time.Now().UnixMilli()
	profile.LastRefreshError = refreshErr.Error()
	if isTerminalRefreshError(refreshErr) {
		profile.Disabled = true
	}
	legacy.Profiles[tm.profileKey] = profile
	for i := range tm.profiles {
		if tm.profiles[i].Key == tm.profileKey {
			tm.profiles[i].Profile = profile
			break
		}
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tm.storePath, data, 0o600)
}
