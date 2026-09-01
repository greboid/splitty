package auth

import (
	"context"
	"net/http"

	"github.com/greboid/splitpayments/internal/user"
)

// withUser attaches the logged-in user to the request context.
func withUser(ctx context.Context, u *user.User) context.Context {
	return WithUser(ctx, u)
}

// WithUser attaches a user to a context, as the login middleware does —
// for callers that build authenticated requests outside that flow.
func WithUser(ctx context.Context, u *user.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// UserFrom returns the logged-in user for the request, or nil when
// unauthenticated.
func UserFrom(r *http.Request) *user.User {
	if u, ok := r.Context().Value(userKey).(*user.User); ok {
		return u
	}
	return nil
}

// RequireAnon redirects already-authenticated users away from the
// setup/login pages.
func RequireAnon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserFrom(r) != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
