// Package render loads the html/template set from the embedded web FS (or a
// developer-supplied override directory) and renders named pages inside the
// shared layout.
//
// Each page template is compiled together with layout.html and the shared
// partials (_*.html) into its own template set, so pages can freely define
// "title", "content" and "scripts" blocks that override the layout's block
// defaults.
package render

import (
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// NavGroup and NavFriend are the minimal views of the user's groups and
// friends shown in the sidebar.
type NavGroup struct {
	ID   int64
	Name string
}

type NavFriend struct {
	ID      int64
	Name    string
	IsGuest bool
}

// Nav carries the sidebar lists for one request.
type Nav struct {
	Groups  []NavGroup
	Friends []NavFriend
}

// navCtxKey is the context key under which the nav middleware attaches the
// sidebar data to authenticated requests.
type navCtxKey struct{}

// WithNav returns ctx carrying the sidebar data for the request.
func WithNav(ctx context.Context, nav Nav) context.Context {
	return context.WithValue(ctx, navCtxKey{}, nav)
}

// Common is embedded in every page's data struct so all templates can rely
// on these fields.
type Common struct {
	CurrentUser User
	LoggedIn    bool
	Currency    string
	Symbol      string
	ScanEnabled bool
	Path        string
	Flash       string
	FlashKind   string // "ok" or "error"
	Nav         Nav    // sidebar lists; empty on anonymous pages
}

// User is the minimal view of the logged-in user needed by templates.
type User struct {
	Name    string
	IsGuest bool
}

// Renderer holds one compiled template set per page plus app-wide settings.
type Renderer struct {
	pages       map[string]*template.Template
	currency    string
	symbol      string
	scanEnabled bool
}

// New parses layout.html, all partials (_*.html) and each page (*.html) into
// per-page template sets. assetVersion, when non-empty, is appended to
// /static URLs via the "static" template func so browsers can cache assets
// immutably and still pick up new versions after a rebuild.
func New(fsys fs.FS, currency, symbol string, scanEnabled bool, assetVersion string) (*Renderer, error) {
	funcs := template.FuncMap{
		"money": func(v int64) string { return format(v, symbol) },
		"moneySigned": func(v int64) string {
			if v > 0 {
				return "+" + format(v, symbol)
			}
			return format(v, symbol)
		},
		"mul":       func(v, n int64) int64 { return v * n },
		"list":      func(items ...string) []string { return items },
		"join":      strings.Join,
		"hasPrefix": strings.HasPrefix,
		"title":     capitalize,
		"dateFmt":   formatDate,
		"datetime":  formatDateTime,
		"static": func(p string) string {
			if assetVersion == "" {
				return "/static" + p
			}
			return "/static" + p + "?v=" + assetVersion
		},
	}
	r := &Renderer{
		pages:       map[string]*template.Template{},
		currency:    currency,
		symbol:      symbol,
		scanEnabled: scanEnabled,
	}

	pages, err := fs.Glob(fsys, "*.html")
	if err != nil {
		return nil, err
	}
	var shared []string
	for _, p := range pages {
		if p == "layout.html" || (len(p) > 0 && p[0] == '_') {
			shared = append(shared, p)
		}
	}
	for _, page := range pages {
		if page == "layout.html" || (len(page) > 0 && page[0] == '_') {
			continue
		}
		files := append([]string{"layout.html"}, shared...)
		files = append(files, page)
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(fsys, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		r.pages[page] = t
	}
	return r, nil
}

// CommonFrom builds the common template fields for a request. currentUser is
// nil on anonymous pages (login, setup, invite).
func (r *Renderer) CommonFrom(req *http.Request, currentUser *User) Common {
	c := Common{
		Currency:    r.currency,
		Symbol:      r.symbol,
		ScanEnabled: r.scanEnabled,
		Path:        req.URL.Path,
	}
	if currentUser != nil {
		c.CurrentUser = *currentUser
		c.LoggedIn = true
	}
	if nav, ok := req.Context().Value(navCtxKey{}).(Nav); ok {
		c.Nav = nav
	}
	c.Flash = req.URL.Query().Get("flash")
	c.FlashKind = req.URL.Query().Get("flash-kind")
	if c.FlashKind != "error" {
		c.FlashKind = "ok"
	}
	return c
}

func format(v int64, symbol string) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%s%d.%02d", sign, symbol, v/100, v%100)
}

// Render writes the named page (e.g. "dashboard.html") wrapped in the
// layout. data must include render.Common (usually via struct embedding).
func (r *Renderer) Render(w http.ResponseWriter, status int, name string, data any) {
	t, ok := r.pages[name]
	if !ok {
		slog.Error("unknown template", "template", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		slog.Error("render template", "template", name, "error", err)
	}
}

// Pages reports the set of compiled page names (used by tests).
func (r *Renderer) Pages() []string {
	out := make([]string, 0, len(r.pages))
	for k := range r.pages {
		out = append(out, k)
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func formatDate(v any) string {
	switch t := v.(type) {
	case string:
		if d, err := time.Parse("2006-01-02", t); err == nil {
			return d.Format("2 Jan 2006")
		}
		return t
	case time.Time:
		return t.Format("2 Jan 2006")
	}
	return fmt.Sprint(v)
}

func formatDateTime(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.Format("2 Jan 2006, 15:04")
	case string:
		if d, err := time.Parse(time.RFC3339, t); err == nil {
			return d.Format("2 Jan 2006, 15:04")
		}
		return t
	}
	return fmt.Sprint(v)
}
