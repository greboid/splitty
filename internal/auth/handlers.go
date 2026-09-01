package auth

// Handlers for first-run setup, login, logout and invite acceptance.
//
// auth deliberately imports no domain store packages: GroupSource and
// ActivitySink are narrow interfaces satisfied by the group and activity
// stores, so domain packages can freely import auth (for UserFrom) without
// cycles.

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/greboid/splitpayments/internal/render"
	"github.com/greboid/splitpayments/internal/user"
)

// GroupSource is the slice of the group store the invite flow needs.
type GroupSource interface {
	GroupName(id int64) (string, error)
	AddMember(groupID, userID int64) error
}

// ActivitySink is the slice of the activity store the invite flow needs.
type ActivitySink interface {
	Append(groupID, expenseID, actorID int64, typ, detail string) error
}

type Handlers struct {
	Users    *user.Store
	Sessions *SessionStore
	Invites  *InviteStore
	Groups   GroupSource
	Activity ActivitySink
	Render   *render.Renderer
}

// safeRedirect allows only same-site paths for post-login redirects.
func safeRedirect(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func redirectFlash(w http.ResponseWriter, r *http.Request, path, flash, kind string) {
	http.Redirect(w, r, path+"?flash="+url.QueryEscape(flash)+"&flash-kind="+kind, http.StatusSeeOther)
}

// --- first-run setup -------------------------------------------------------

func (h *Handlers) Setup(w http.ResponseWriter, r *http.Request) {
	n, err := h.Users.Count()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		h.setupPost(w, r)
		return
	}
	h.Render.Render(w, http.StatusOK, "setup.html", setupData{Common: h.Render.CommonFrom(r, nil)})
}

type setupData struct {
	render.Common
	Error string
}

func (h *Handlers) setupPost(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	if name == "" || email == "" || len(password) < 8 {
		h.Render.Render(w, http.StatusUnprocessableEntity, "setup.html", setupData{
			Common: h.Render.CommonFrom(r, nil),
			Error:  "Please fill in your name, email, and a password of at least 8 characters.",
		})
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		http.Error(w, "hashing error", http.StatusInternalServerError)
		return
	}
	u := user.User{Name: name, Email: email, PasswordHash: hash}
	if err := h.Users.Create(&u); err != nil {
		slog.Error("create first user", "error", err)
		h.Render.Render(w, http.StatusUnprocessableEntity, "setup.html", setupData{
			Common: h.Render.CommonFrom(r, nil),
			Error:  "That email may already be in use.",
		})
		return
	}
	if err := h.Sessions.Login(w, u.ID); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	slog.Info("initial account created")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- login / logout --------------------------------------------------------

type loginData struct {
	render.Common
	Next  string
	Error string
}

func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		h.loginPost(w, r)
		return
	}
	h.Render.Render(w, http.StatusOK, "login.html", loginData{
		Common: h.Render.CommonFrom(r, nil),
		Next:   r.URL.Query().Get("next"),
	})
}

func (h *Handlers) loginPost(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	next := safeRedirect(r.PostFormValue("next"))

	fail := func() {
		h.Render.Render(w, http.StatusUnprocessableEntity, "login.html", loginData{
			Common: h.Render.CommonFrom(r, nil), Next: next,
			Error: "No account matches that email and password.",
		})
	}
	u, err := h.Users.ByEmail(email)
	if errors.Is(err, user.ErrNotFound) {
		fail()
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if u.PasswordHash == "" || !VerifyPassword(u.PasswordHash, password) {
		fail()
		return
	}
	if err := h.Sessions.Login(w, u.ID); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	h.Sessions.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- invites ----------------------------------------------------------------

type inviteData struct {
	render.Common
	Invite       Invite
	HasInvite    bool
	ExistingUser bool
	Name         string // prefill for the claim form
	Error        string
}

func (h *Handlers) Invite(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	invite, err := h.Invites.GetByToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := inviteData{Common: h.Render.CommonFrom(r, nil), Invite: invite, HasInvite: true}

	switch {
	case invite.Accepted():
		h.renderInvite(w, http.StatusOK, d)
		return
	case invite.Expired():
		h.renderInvite(w, http.StatusOK, d)
		return
	}

	existing, err := h.Users.ByEmail(invite.Email)
	exists := err == nil && !existing.IsGuest
	if err != nil && !errors.Is(err, user.ErrNotFound) {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	if exists {
		// A full account already owns this email: it must log in, and the
		// invite is consumed on arrival as that user.
		if me := UserFrom(r); me != nil && me.ID == existing.ID {
			h.acceptInvite(w, r, invite, existing.ID, existing.Name)
			return
		}
		d.ExistingUser = true
		h.renderInvite(w, http.StatusOK, d)
		return
	}

	if r.Method == http.MethodPost {
		h.invitePost(w, r, invite, d)
		return
	}
	if err == nil && existing.IsGuest {
		d.Name = existing.Name
	}
	h.renderInvite(w, http.StatusOK, d)
}

func (h *Handlers) renderInvite(w http.ResponseWriter, status int, d inviteData) {
	h.Render.Render(w, status, "invite.html", d)
}

func (h *Handlers) invitePost(w http.ResponseWriter, r *http.Request, invite Invite, d inviteData) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	password := r.PostFormValue("password")
	if name == "" || len(password) < 8 {
		d.Error = "Please set your name and a password of at least 8 characters."
		d.Name = name
		h.renderInvite(w, http.StatusUnprocessableEntity, d)
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		http.Error(w, "hashing error", http.StatusInternalServerError)
		return
	}

	// Upgrade an existing guest with this email, else create a new account.
	guest, gerr := h.Users.ByEmail(invite.Email)
	var (
		u   user.User
		uid int64
	)
	switch {
	case gerr == nil && guest.IsGuest:
		if err := h.Users.UpdateCredentials(guest.ID, name, invite.Email, hash); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		uid = guest.ID
	default:
		u = user.User{Name: name, Email: invite.Email, PasswordHash: hash,
			InvitedBy: sql.NullInt64{Int64: invite.CreatedBy, Valid: true}}
		if err := h.Users.Create(&u); err != nil {
			slog.Error("invite create user", "error", err)
			d.Error = "Could not create the account; the email may already be registered."
			h.renderInvite(w, http.StatusUnprocessableEntity, d)
			return
		}
		uid = u.ID
	}

	if err := h.Sessions.Login(w, uid); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	h.acceptInvite(w, r, invite, uid, name)
}

// acceptInvite consumes the invite, joins the group if any, logs activity
// and redirects to the destination.
func (h *Handlers) acceptInvite(w http.ResponseWriter, r *http.Request, invite Invite, userID int64, name string) {
	if err := h.Invites.Accept(invite.ID, userID); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if invite.GroupID.Valid {
		gid := invite.GroupID.Int64
		if err := h.Groups.AddMember(gid, userID); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		gname, _ := h.Groups.GroupName(gid)
		h.Activity.Append(gid, 0, userID, "joined_group", name+" joined "+gname)
		redirectFlash(w, r, "/", "Welcome, "+name+"! You have joined "+gname+".", "ok")
		return
	}
	redirectFlash(w, r, "/", "Welcome, "+name+"! Your account is ready.", "ok")
}
