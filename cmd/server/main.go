package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/openclaw/customai-gateway-go/internal/cursor"
	httpapi "github.com/openclaw/customai-gateway-go/internal/http"
	"github.com/openclaw/customai-gateway-go/internal/requestid"
)

func main() {
	_ = godotenv.Load()
	port := getEnv("PORT", "8002")

	cfg := cursor.Config{
		APIURL:              getEnv("CUSTOMAI_API_URL", "https://cloudfren.com/backend-api/codex/responses"),
		AuthToken:           strings.TrimSpace(os.Getenv("CUSTOMAI_AUTH_TOKEN")),
		RefreshToken:        strings.TrimSpace(os.Getenv("CUSTOMAI_REFRESH_TOKEN")),
		TokenURL:            strings.TrimSpace(os.Getenv("CUSTOMAI_TOKEN_URL")),
		OAuthClientID:       strings.TrimSpace(os.Getenv("CUSTOMAI_OAUTH_CLIENT_ID")),
		TokenScopes:         parseCommaOrSpaceList(os.Getenv("CUSTOMAI_TOKEN_SCOPES")),
		TokenExpiresAt:      parseUnixMillisEnv(os.Getenv("CUSTOMAI_TOKEN_EXPIRES_AT")),
		RefreshBuffer:       parseDurationSeconds(os.Getenv("CUSTOMAI_REFRESH_BUFFER_SECONDS"), 300),
		TokenStorePath:      strings.TrimSpace(getEnv("CUSTOMAI_TOKEN_STORE_PATH", ".customai-tokens.json")),
		Cookie:              strings.TrimSpace(os.Getenv("CUSTOMAI_COOKIE")),
		RequestTTL:          parseDurationSeconds(os.Getenv("CUSTOMAI_TIMEOUT"), 180),
		DefaultInstructions: strings.TrimSpace(getEnv("CUSTOMAI_DEFAULT_INSTRUCTIONS", "You are a helpful coding assistant.")),
		LogPayload:          parseBoolEnv(os.Getenv("CUSTOMAI_LOG_PAYLOAD")),
		PayloadLogMaxChars:  parseIntEnv(os.Getenv("CUSTOMAI_LOG_PAYLOAD_MAX_CHARS"), 4000),
		ExtraHeaders: parseExtraHeaders(
			os.Getenv("CUSTOMAI_EXTRA_HEADERS"),
			map[string]string{
				"Origin":          strings.TrimSpace(os.Getenv("CUSTOMAI_UPSTREAM_ORIGIN")),
				"Referer":         strings.TrimSpace(os.Getenv("CUSTOMAI_UPSTREAM_REFERER")),
				"User-Agent":      strings.TrimSpace(os.Getenv("CUSTOMAI_UPSTREAM_USER_AGENT")),
				"Accept-Language": strings.TrimSpace(os.Getenv("CUSTOMAI_UPSTREAM_ACCEPT_LANGUAGE")),
			},
		),
	}
	if allowed := os.Getenv("CUSTOMAI_ALLOWED_MODELS"); allowed != "" {
		for _, s := range strings.Split(allowed, ",") {
			if t := strings.TrimSpace(s); t != "" {
				cfg.AllowedModels = append(cfg.AllowedModels, t)
			}
		}
	}

	customClient := cursor.NewClient(cfg)
	gatewayAPIKey := strings.TrimSpace(os.Getenv("CUSTOMAI_GATEWAY_API_KEY"))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	openAIHandler := httpapi.RequireBearer(gatewayAPIKey, httpapi.NewChatCompletionsHandler(customClient))
	mux.Handle("/v1/chat/completions", openAIHandler)
	mux.Handle("/chat/completions", openAIHandler)
	// Some clients/providers call /responses for gpt-5 style models.
	// We accept it and normalize body via the same handler.
	mux.Handle("/v1/responses", openAIHandler)
	mux.Handle("/responses", openAIHandler)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("customai-gateway-go listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("server shutdown error: %v", err)
	} else {
		log.Println("server gracefully stopped")
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDurationSeconds(raw string, defSec int) time.Duration {
	if raw == "" {
		return time.Duration(defSec) * time.Second
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		return time.Duration(defSec) * time.Second
	}
	return time.Duration(sec) * time.Second
}

func parseBoolEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parseIntEnv(raw string, def int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func parseUnixMillisEnv(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func parseCommaOrSpaceList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = strings.ReplaceAll(raw, ",", " ")
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseExtraHeaders parses "Key: Value||Key2: Value2" and merges with explicit headers.
func parseExtraHeaders(raw string, explicit map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range explicit {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, "||") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(part, ":")
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:idx])
		v := strings.TrimSpace(part[idx+1:])
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	return out
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := requestid.Normalize(r.Header.Get("X-Request-Id"))
		w.Header().Set("X-Request-Id", reqID)
		r = r.WithContext(requestid.WithContext(r.Context(), reqID))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf(
			"[gateway] request_id=%s method=%s path=%s status=%d duration_ms=%d remote=%s ua=%q",
			reqID,
			r.Method,
			r.URL.Path,
			rec.status,
			time.Since(start).Milliseconds(),
			r.RemoteAddr,
			r.UserAgent(),
		)
	})
}
