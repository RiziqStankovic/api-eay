package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

type contextKey struct{}

func New() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "gw_unknown"
	}
	return "gw_" + hex.EncodeToString(b)
}

func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return New()
	}
	return raw
}

func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
