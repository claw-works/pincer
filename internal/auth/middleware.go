package auth

import (
	"context"
	"net/http"

	"github.com/claw-works/claw-hub/internal/project"
)

type contextKey string

const UserContextKey contextKey = "auth_user"

// Middleware validates X-API-Key header.
// If the key is valid, injects the User into the request context.
// If the key is missing or invalid, returns 401.
func Middleware(store *project.PGStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				http.Error(w, `{"error":"X-API-Key required"}`, http.StatusUnauthorized)
				return
			}
			user, err := store.GetUserByAPIKey(r.Context(), apiKey)
			if err != nil {
				http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext extracts the authenticated user from context.
// Returns nil if not authenticated.
func FromContext(ctx context.Context) *project.User {
	u, _ := ctx.Value(UserContextKey).(*project.User)
	return u
}
