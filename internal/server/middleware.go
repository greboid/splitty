package server

// The middleware chain assembles so that the last middleware listed is the
// outermost: a request passes the list from back to front. The effective
// order is:
//
//	ErrorHandler → StripTrailingSlashes → CacheControl → Compress →
//	CrossOriginProtection (CSRF) → SetupRedirect → Headers(CSP…) → HSTS →
//	Sessions → nav data → RealAddress → slog request log → mux

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/csmith/middleware"

	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/render"
)

// chain assembles the middleware stack around the mux.
func (s *Server) chain(mux http.Handler) http.Handler {
	trusted := defaultTrustedRanges()
	for _, cidr := range s.Cfg.TrustedProxies {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			trusted = append(trusted, *ipnet)
		} else {
			slog.Warn("ignoring invalid trusted-proxy CIDR", "cidr", cidr)
		}
	}

	mws := []func(http.Handler) http.Handler{
		requestLogger(),
		s.withNav,
		middleware.RealAddress(middleware.WithTrustedProxies(trusted)),
		s.Sessions.Middleware,
	}
	if s.Cfg.CookieSecure {
		// HTTPS termination happens behind a reverse proxy in this mode.
		mws = append(mws, middleware.HSTS(middleware.WithHSTSReverseProxy()))
	}
	mws = append(mws,
		middleware.Headers(
			middleware.WithHeader("X-Content-Type-Options", "nosniff"),
			middleware.WithHeader("X-Frame-Options", "DENY"),
			middleware.WithHeader("Referrer-Policy", "same-origin"),
			// Zero inline script/style anywhere in the templates, so
			// 'self' everywhere is enough; data: and blob: img-src are
			// for the receipt preview (scan.js uses URL.createObjectURL).
			middleware.WithHeader("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"),
		),
		s.needsSetup,
		middleware.CrossOriginProtection(),
		middleware.Compress(),
		middleware.CacheControl(),
		middleware.StripTrailingSlashes(),
		middleware.ErrorHandler(
			middleware.WithErrorHandler(http.StatusNotFound, s.errorPage(404)),
			middleware.WithErrorHandler(http.StatusInternalServerError, s.errorPage(500)),
			middleware.WithErrorHandler(http.StatusForbidden, s.errorPage(403)),
		),
	)
	return middleware.Chain(middleware.WithMiddleware(mws...))(mux)
}

// needsSetup redirects every page to /setup while the database has no
// accounts, so a fresh instance lands on first-run setup without a manual
// visit. Health checks, static assets and the PWA plumbing stay reachable
// (the setup page needs them); once a user exists everything passes
// through, and /setup flips itself to /login.
func (s *Server) needsSetup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/setup" || p == "/healthz" || p == "/sw.js" ||
			p == "/manifest.webmanifest" || strings.HasPrefix(p, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		n, err := s.Users.Count()
		if err != nil {
			slog.Error("setup check", "error", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if n == 0 {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withNav loads the sidebar lists (groups and friends) for logged-in users
// and attaches them to the request context, so every rendered page can show
// them. Anonymous requests and failures leave the nav empty rather than
// blocking the page.
func (s *Server) withNav(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := auth.UserFrom(r); u != nil {
			var nav render.Nav
			groups, err := s.Groups.ForUser(u.ID)
			if err != nil {
				slog.Error("nav groups", "error", err)
			}
			for _, g := range groups {
				nav.Groups = append(nav.Groups, render.NavGroup{ID: g.ID, Name: g.Name})
			}
			friends, err := s.Groups.Friends(u.ID)
			if err != nil {
				slog.Error("nav friends", "error", err)
			}
			for _, f := range friends {
				nav.Friends = append(nav.Friends, render.NavFriend{ID: f.ID, Name: f.Name, IsGuest: f.IsGuest})
			}
			r = r.WithContext(render.WithNav(r.Context(), nav))
		}
		next.ServeHTTP(w, r)
	})
}

func defaultTrustedRanges() []net.IPNet {
	ranges := []string{
		"10.0.0.0/8", "127.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7",
	}
	var out []net.IPNet
	for _, cidr := range ranges {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, *ipnet)
		}
	}
	return out
}

// requestLogger is a small slog-based request log; the middleware package's
// TextLog only does Apache format.
func requestLogger() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			slog.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// errorPage renders friendly error pages for the ErrorHandler middleware.
// The user is unknown here (ErrorHandler is innermost), so the page is
// rendered without navigation state.
func (s *Server) errorPage(status int) http.Handler {
	msg := map[int]string{
		403: "That request isn't allowed.",
		404: "That page doesn't exist.",
		500: "Something went wrong on our side. Please try again.",
	}[status]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if renderAcceptsHTML(r) {
			s.Render.Render(w, status, "error.html", struct {
				render.Common
				Message string
			}{Common: s.Render.CommonFrom(r, nil), Message: msg})
			return
		}
		http.Error(w, msg, status)
	})
}

func renderAcceptsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return accept == "" || len(accept) >= 4 && accept[:4] == "text"
}
