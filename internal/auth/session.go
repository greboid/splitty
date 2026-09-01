package auth

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/greboid/splitpayments/internal/user"
)

const sessionCookie = "sp_session"

// SessionStore persists session tokens (hashed) and resolves them to users.
type SessionStore struct {
	DB       *sql.DB
	Lifetime time.Duration
	Secure   bool
}

type session struct {
	tokenHash string
	expiresAt time.Time
}

func (s *SessionStore) Create(userID int64) (token string, err error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hash, userID, time.Now().Add(s.Lifetime))
	return token, err
}

// User returns the account for the presented session token, or ErrNotFound
// when the session is unknown or expired (expired rows are pruned lazily).
func (s *SessionStore) User(token string) (user.User, error) {
	var u user.User
	var email, hash sql.NullString
	var expires time.Time
	err := s.DB.QueryRow(`
		SELECT u.id, u.email, u.name, u.password_hash, u.is_guest, u.invited_by, u.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?`, HashToken(token),
	).Scan(&u.ID, &email, &u.Name, &hash, &u.IsGuest, &u.InvitedBy, &u.CreatedAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return u, user.ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.Email = email.String
	u.PasswordHash = hash.String
	if time.Now().After(expires) {
		s.DB.Exec(`DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
		return u, user.ErrNotFound
	}
	return u, nil
}

func (s *SessionStore) Delete(token string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

// setCookie writes the session cookie (HttpOnly, SameSite=Lax, Secure per
// config).
func (s *SessionStore) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

type ctxKey int

const userKey ctxKey = 0

// Middleware loads the session cookie into the request context (as a
// *user.User, nil when absent) and prunes it if stale.
func (s *SessionStore) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			if u, err := s.User(c.Value); err == nil {
				next.ServeHTTP(w, r.WithContext(withUser(r.Context(), &u)))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser redirects unauthenticated requests to the login page,
// preserving the destination for post-login redirect.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserFrom(r) == nil {
			dest := r.URL.Path
			if r.URL.RawQuery != "" {
				dest += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(dest), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Login establishes a session for the user and sets the cookie.
func (s *SessionStore) Login(w http.ResponseWriter, userID int64) error {
	token, err := s.Create(userID)
	if err != nil {
		return err
	}
	s.setCookie(w, token, int(s.Lifetime.Seconds()))
	return nil
}

// Logout deletes the session row and clears the cookie.
func (s *SessionStore) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.Delete(c.Value); err != nil {
			slog.Error("delete session", "error", err)
		}
	}
	s.setCookie(w, "", -1)
}
