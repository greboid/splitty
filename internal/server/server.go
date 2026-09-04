// Package server assembles the HTTP mux, the middleware chain and the
// pages that don't belong to a single domain package: the dashboard,
// friends (direct ledgers) and CSV export.
package server

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/greboid/splitpayments/internal/activity"
	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/balance"
	"github.com/greboid/splitpayments/internal/config"
	"github.com/greboid/splitpayments/internal/expense"
	"github.com/greboid/splitpayments/internal/group"
	"github.com/greboid/splitpayments/internal/receipt"
	"github.com/greboid/splitpayments/internal/render"
	"github.com/greboid/splitpayments/internal/user"
	"github.com/greboid/splitpayments/web"
)

type Server struct {
	Cfg       config.Config
	Users     *user.Store
	Sessions  *auth.SessionStore
	AuthH     *auth.Handlers
	Invites   *auth.InviteStore
	Groups    *group.Store
	GroupH    *group.Handlers
	Expenses  *expense.Store
	ExpenseH  *expense.Handlers
	Activity  *activity.Store
	ActivityH *activity.Handler
	Render    *render.Renderer
	ReceiptH  *receipt.Handlers
}

// Handler builds the fully-wired http.Handler for the app.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health check, no auth.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	})

	// Anonymous pages (setup/login also bounce logged-in users away).
	mux.Handle("GET /setup", auth.RequireAnon(http.HandlerFunc(s.AuthH.Setup)))
	mux.Handle("POST /setup", auth.RequireAnon(http.HandlerFunc(s.AuthH.Setup)))
	mux.Handle("GET /login", http.HandlerFunc(s.AuthH.Login))
	mux.Handle("POST /login", http.HandlerFunc(s.AuthH.Login))
	mux.Handle("GET /invite/{token}", http.HandlerFunc(s.AuthH.Invite))
	mux.Handle("POST /invite/{token}", http.HandlerFunc(s.AuthH.Invite))

	// Static assets.
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.staticHandler()))

	// PWA plumbing: the service worker must live at the site root to get
	// a "/" scope; the manifest tells Android/Chrome how to install the
	// app. Both are anonymous and always revalidated.
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		s.staticFile(w, "sw.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		s.staticFile(w, "manifest.webmanifest", "application/manifest+json")
	})

	// Authenticated app.
	authed := auth.RequireUser
	mux.Handle("GET /{$}", authed(http.HandlerFunc(s.dashboard)))
	mux.Handle("POST /logout", http.HandlerFunc(s.AuthH.Logout))
	mux.Handle("GET /groups", authed(http.HandlerFunc(s.GroupH.List)))
	mux.Handle("POST /groups", authed(http.HandlerFunc(s.GroupH.Create)))
	mux.Handle("GET /groups/{id}", authed(http.HandlerFunc(s.GroupH.Detail)))
	mux.Handle("POST /groups/{id}/members", authed(http.HandlerFunc(s.GroupH.AddMember)))
	mux.Handle("POST /groups/{id}/settings", authed(http.HandlerFunc(s.GroupH.Settings)))
	mux.Handle("POST /groups/{id}/settle", authed(http.HandlerFunc(s.GroupH.Settle)))
	mux.Handle("GET /groups/{id}/export.csv", authed(http.HandlerFunc(s.exportCSV)))
	mux.Handle("GET /groups/{id}/expenses/new", authed(http.HandlerFunc(s.ExpenseH.New)))
	mux.Handle("GET /drafts/{id}", authed(http.HandlerFunc(s.ExpenseH.ViewDraft)))
	mux.Handle("PUT /drafts/{id}", authed(http.HandlerFunc(s.ExpenseH.SaveDraftForm)))
	mux.Handle("POST /drafts/{id}/clear", authed(http.HandlerFunc(s.ExpenseH.ClearDraft)))
	mux.Handle("POST /drafts/{id}/discard", authed(http.HandlerFunc(s.ExpenseH.DiscardDraft)))
	mux.Handle("POST /drafts/{id}/submit", authed(http.HandlerFunc(s.ExpenseH.SubmitDraft)))
	mux.Handle("GET /expenses/{id}", authed(http.HandlerFunc(s.ExpenseH.Detail)))
	mux.Handle("GET /expenses/{id}/edit", authed(http.HandlerFunc(s.ExpenseH.Edit)))
	mux.Handle("POST /expenses/{id}/edit", authed(http.HandlerFunc(s.ExpenseH.Edit)))
	mux.Handle("POST /expenses/{id}/delete", authed(http.HandlerFunc(s.ExpenseH.Delete)))
	mux.Handle("GET /friends", authed(http.HandlerFunc(s.friendsList)))
	mux.Handle("POST /friends", authed(http.HandlerFunc(s.friendsAdd)))
	mux.Handle("GET /friends/{id}", authed(http.HandlerFunc(s.friendDetail)))
	mux.Handle("GET /activity", authed(s.ActivityH))
	mux.Handle("POST /receipts", authed(http.HandlerFunc(s.ReceiptH.Upload)))
	mux.Handle("POST /receipts/{file}/analyze", authed(http.HandlerFunc(s.ReceiptH.Analyze)))
	mux.Handle("GET /receipts/{file}", authed(http.HandlerFunc(s.ReceiptH.Serve)))

	return s.chain(mux)
}

// staticFile serves one file from the embedded static FS (or -static-dir in
// dev) with an explicit content type and no caching, so the browser always
// revalidates the service worker and manifest. The query string is ignored,
// which is what lets /sw.js?v=... bust worker updates per build.
func (s *Server) staticFile(w http.ResponseWriter, name, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	var f fs.File
	var err error
	if s.Cfg.StaticDir != "" {
		f, err = os.Open(filepath.Join(s.Cfg.StaticDir, name))
	} else {
		f, err = web.Static().Open(name)
	}
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	io.Copy(w, f)
}

// staticHandler serves assets from -static-dir when set (dev), else the
// embedded FS.
func (s *Server) staticHandler() http.Handler {
	if s.Cfg.StaticDir != "" {
		fs := http.FileServer(http.Dir(s.Cfg.StaticDir))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			fs.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Immutable content: the embedded files never change for a binary.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.FileServerFS(web.Static()).ServeHTTP(w, r)
	})
}

// --- shared helpers -----------------------------------------------------------

// directPosition computes what a owes b and b owes a within one pair's
// ledger. Net is additive, so the flat share rows form one synthetic
// expense for the balance engine.
func directPosition(brows []expense.BalanceRow, a, b int64) (aOwesB, bOwesA int64) {
	var shares []balance.Share
	for _, br := range brows {
		shares = append(shares, balance.Share{UserID: br.UserID, Paid: br.Paid, Owed: br.Owed})
	}
	for _, d := range balance.Pairwise([]balance.Expense{{Shares: shares}}) {
		switch {
		case d.From == a && d.To == b:
			aOwesB += d.Amount
		case d.From == b && d.To == a:
			bOwesA += d.Amount
		}
	}
	return aOwesB, bOwesA
}

// --- dashboard ------------------------------------------------------------------

type dashboardPage struct {
	render.Common
	TotalBalance int64 // user's net across everything (positive = owed to you)
}

// dashboard renders the total-balance chip: the user's net across all groups
// plus their direct (friend) ledgers.
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	page := dashboardPage{Common: s.Render.CommonFrom(r, &render.User{Name: u.Name, IsGuest: u.IsGuest})}

	groups, err := s.Groups.ForUser(u.ID)
	if err != nil {
		slog.Error("dashboard groups", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	for _, g := range groups {
		if brows, err := s.Expenses.BalanceRows(g.ID); err == nil {
			for _, br := range brows {
				if br.UserID == u.ID {
					page.TotalBalance += br.Paid - br.Owed
				}
			}
		}
	}

	friends, err := s.Groups.Friends(u.ID)
	if err != nil {
		slog.Error("dashboard friends", "error", err)
	}
	for _, f := range friends {
		if g, err := s.Groups.DirectBetween(u.ID, f.ID); err == nil {
			if brows, err := s.Expenses.BalanceRows(g.ID); err == nil {
				youOwe, owesYou := directPosition(brows, u.ID, f.ID)
				page.TotalBalance += owesYou - youOwe
			}
		}
	}

	s.Render.Render(w, http.StatusOK, "dashboard.html", page)
}

// --- friends ------------------------------------------------------------------

type friendRow struct {
	User    user.User
	YouOwe  int64
	OwesYou int64
}

func (s *Server) friendsList(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	friends, err := s.Groups.Friends(u.ID)
	if err != nil {
		slog.Error("friends list", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var rows []friendRow
	for _, f := range friends {
		row := friendRow{User: f}
		if g, err := s.Groups.DirectBetween(u.ID, f.ID); err == nil {
			if brows, err := s.Expenses.BalanceRows(g.ID); err == nil {
				row.YouOwe, row.OwesYou = directPosition(brows, u.ID, f.ID)
			}
		}
		rows = append(rows, row)
	}
	s.Render.Render(w, http.StatusOK, "friends.html", struct {
		render.Common
		Friends []friendRow
	}{Common: s.Render.CommonFrom(r, &render.User{Name: u.Name, IsGuest: u.IsGuest}), Friends: rows})
}

func (s *Server) friendDetail(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	friendID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || friendID == u.ID {
		http.Redirect(w, r, "/friends", http.StatusSeeOther)
		return
	}
	friend, err := s.Users.ByID(friendID)
	if err != nil {
		s.notFound(w, r)
		return
	}
	// Lazily creates the hidden per-pair ledger on first visit.
	g, err := s.Groups.DirectBetween(u.ID, friendID)
	if err != nil {
		slog.Error("direct ledger", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	expenses, err := s.Expenses.ListForGroup(g.ID)
	if err != nil {
		slog.Error("friend expenses", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	bexp := group.BalanceExpenses(expenses)
	net := balance.Net(bexp)
	var youOwe, owesYou int64
	for _, d := range balance.Pairwise(bexp) {
		if d.From == u.ID && d.To == friendID {
			youOwe = d.Amount
		}
		if d.From == friendID && d.To == u.ID {
			owesYou = d.Amount
		}
	}

	type expenseRow struct {
		Expense    expense.Expense
		PaidBy     string
		YourImpact int64
	}
	var rows []expenseRow
	for _, e := range expenses {
		row := expenseRow{Expense: e}
		for _, sh := range e.Shares {
			if sh.Paid > 0 {
				if sh.UserID == u.ID {
					row.PaidBy = "You"
				} else {
					row.PaidBy = friend.Name
				}
			}
			if sh.UserID == u.ID {
				row.YourImpact = sh.Paid - sh.Owed
			}
		}
		rows = append(rows, row)
	}

	s.Render.Render(w, http.StatusOK, "friend.html", struct {
		render.Common
		Friend   user.User
		MeID     int64
		GroupID  int64
		YouOwe   int64
		OwesYou  int64
		YourNet  int64
		Expenses []expenseRow
		Activity []activity.Entry
	}{
		Common:   s.Render.CommonFrom(r, &render.User{Name: u.Name, IsGuest: u.IsGuest}),
		Friend:   friend,
		MeID:     u.ID,
		GroupID:  g.ID,
		YouOwe:   youOwe,
		OwesYou:  owesYou,
		YourNet:  net[u.ID],
		Expenses: rows,
		Activity: s.groupActivity(g.ID),
	})
}

func (s *Server) groupActivity(groupID int64) []activity.Entry {
	entries, err := s.Activity.ForGroup(groupID, 30)
	if err != nil {
		slog.Error("activity", "error", err)
		return nil
	}
	return entries
}

// --- CSV export ---------------------------------------------------------------

// exportCSV serves GET /groups/{id}/export.csv in a Splitwise-like shape:
// one row per expense/payment, one paid+owed column pair per member.
func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	g, err := s.Groups.ByID(id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if ok, _ := s.Groups.IsMember(g.ID, u.ID); !ok {
		s.notFound(w, r)
		return
	}
	members, err := s.Groups.Members(g.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	expenses, err := s.Expenses.ListForGroup(g.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s-export.csv"`, slugify(g.Name)))
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	header := []string{"Date", "Description", "Category", "Notes", "Currency"}
	for _, m := range members {
		header = append(header, m.Name+" paid", m.Name+" owed")
	}
	cw.Write(header)

	for _, e := range expenses {
		row := []string{e.Date, e.Description, e.Category, e.Notes, s.Cfg.Currency}
		for _, m := range members {
			var paid, owed string
			for _, sh := range e.Shares {
				if sh.UserID == m.ID {
					paid = formatMinor(sh.Paid)
					owed = formatMinor(sh.Owed)
				}
			}
			row = append(row, paid, owed)
		}
		cw.Write(row)
	}
	cw.Flush()
}

func formatMinor(v int64) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "group"
	}
	return b.String()
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.Render.Render(w, http.StatusNotFound, "error.html", struct {
		render.Common
		Message string
	}{Common: s.Render.CommonFrom(r, nil), Message: "That page doesn't exist, or you don't have access to it."})
}

// friendsAdd serves POST /friends: resolve an identity to a user and open
// (or reuse) the direct ledger with them. An email that belongs to an
// account links straight away; an unknown email gets an invite link — the
// pending friend is created as an email-named placeholder that the invite
// upgrades in place once claimed. Anything else becomes a name-only guest.
func (s *Server) friendsAdd(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r)
	ident := strings.TrimSpace(r.PostFormValue("identity"))
	if ident == "" {
		redirectFlash(w, r, "/friends", "Enter an email or name.", "error")
		return
	}

	var friend user.User
	if strings.Contains(ident, "@") {
		email := strings.ToLower(ident)
		existing, err := s.Users.ByEmail(email)
		switch {
		case err == nil && existing.ID == u.ID:
			redirectFlash(w, r, "/friends", "That's your own email address.", "error")
			return
		case err == nil && !existing.IsGuest:
			friend = existing
		default:
			if err != nil && !errors.Is(err, user.ErrNotFound) {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			s.inviteFriend(w, r, u, email)
			return
		}
	} else {
		guest, err := s.findOrCreateGuest(ident, u.ID)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		friend = guest
	}

	if _, err := s.Groups.DirectBetween(u.ID, friend.ID); err != nil {
		slog.Error("direct ledger", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/friends/%d", friend.ID), http.StatusSeeOther)
}

// inviteFriend opens the ledger with a not-yet-registered email and flashes
// an invite link for it. Re-adding the same email re-issues a fresh link
// (e.g. when the first one was lost) without duplicating the friend.
func (s *Server) inviteFriend(w http.ResponseWriter, r *http.Request, u *user.User, email string) {
	friend, err := s.findOrCreateInvitedGuest(email, u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	ledger, err := s.Groups.DirectBetween(u.ID, friend.ID)
	if err != nil {
		slog.Error("direct ledger", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	token, err := s.Invites.Create(email, 0, u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	s.Activity.Append(ledger.ID, 0, u.ID, activity.MemberAdded, "An invite was sent to "+email)
	link := baseURL(r) + "/invite/" + token
	redirectFlash(w, r, "/friends", "Invite created — send this link: "+link, "ok")
}

// findOrCreateInvitedGuest resolves the placeholder friend for an invited
// email, creating one named by the email if needed. Claiming the invite
// later upgrades this same row to a full account, keeping the ledger.
func (s *Server) findOrCreateInvitedGuest(email string, invitedBy int64) (user.User, error) {
	existing, err := s.Users.ByEmail(email)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, user.ErrNotFound) {
		return user.User{}, err
	}
	g := user.User{Name: email, Email: email, IsGuest: true,
		InvitedBy: sql.NullInt64{Int64: invitedBy, Valid: true}}
	if err := s.Users.Create(&g); err != nil {
		return user.User{}, err
	}
	return g, nil
}

// findOrCreateGuest resolves a guest by exact name, creating one if needed.
func (s *Server) findOrCreateGuest(name string, invitedBy int64) (user.User, error) {
	guest, found, err := s.Users.GuestByName(name)
	if err != nil {
		return user.User{}, err
	}
	if found {
		return guest, nil
	}
	g := user.User{Name: name, IsGuest: true,
		InvitedBy: sql.NullInt64{Int64: invitedBy, Valid: true}}
	if err := s.Users.Create(&g); err != nil {
		return user.User{}, err
	}
	return g, nil
}

// redirectFlash mirrors the auth/group helpers without an import cycle.
func redirectFlash(w http.ResponseWriter, r *http.Request, path, flash, kind string) {
	http.Redirect(w, r, path+"?flash="+url.QueryEscape(flash)+"&flash-kind="+kind, http.StatusSeeOther)
}

// baseURL mirrors group.baseURL: scheme+host for invite links.
func baseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	}
	return scheme + "://" + r.Host
}
