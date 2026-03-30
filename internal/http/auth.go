package http

import (
	"net/http"
	"strings"
)

// RequireBearer returns a handler that checks Authorization: Bearer <token> when expectedToken is non-empty.
// If expectedToken is empty, the inner handler is called without check (auth disabled).
// LiteLLM/OpenWebUI can use this by setting provider api key to CUSTOMAI_GATEWAY_API_KEY.
func RequireBearer(expectedToken string, next http.Handler) http.Handler {
	if strings.TrimSpace(expectedToken) == "" {
		return next
	}
	token := strings.TrimSpace(expectedToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if auth == "" {
			writeError(w, http.StatusUnauthorized, "missing Authorization header")
			return
		}
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeError(w, http.StatusUnauthorized, "Authorization must be Bearer <token>")
			return
		}
		got := strings.TrimSpace(auth[len(prefix):])
		if got != token {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
