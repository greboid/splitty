package group

// Page handlers: list/create groups, the group detail page (balances,
// expenses, members, invites, activity, settle-up), member management and
// settings.

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/greboid/splitpayments/internal/activity"
	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/balance"
	"github.com/greboid/splitpayments/internal/expense"
	"github.com/greboid/splitpayments/internal/money"
	"github.com/greboid/splitpayments/internal/render"
	"github.com/greboid/splitpayments/internal/user"
)

type Handlers struct {
	Store    *Store
	Users    *user.Store
	Expenses *expense.Store
	Activity *activity.Store
	Invites  *auth.InviteStore
	Render   *render.Renderer
}

func (h *Handlers) currentUser(r *http.Request) *user.User { return auth.UserFrom(r) }

func (h *Handlers) common(r *http.Request, u *user.User) render.Common {
	return h.Render.CommonFrom(r, &render.User{Name: u.Name, IsGuest: u.IsGuest})
}

type debtView struct {
	FromName string
	ToName   string
	Amount   int64
}

type expenseRow struct {
	Expense    expense.Expense
	PaidBy     string // comma-joined payer names
	YourPaid   int64
	YourOwed   int64
	YourImpact int64 // paid - owed
}

type memberRow struct {
	User    user.User
	Net     int64 // member's net position in the group
	YouOwe  int64 // current user owes this member
	OwesYou int64 // this member owes the current user
	IsYou   bool
}

type groupPage struct {
	render.Common
	Group      Group
	Members    []memberRow
	Debts      []debtView
	Expenses   []expenseRow
	Pending    []auth.Invite
	Activity   []activity.Entry
	CanSettle  bool
	TotalSpend int64
	DraftID    string // the current user's unfinished expense draft, if any
}

// balanceExpenses converts stored expenses to the balance engine's shape.
// BalanceExpenses converts stored expenses to the balance engine shape;
// exported for the server package (friend ledgers, CSV).
func BalanceExpenses(expenses []expense.Expense) []balance.Expense {
	out := make([]balance.Expense, len(expenses))
	for i, e := range expenses {
		shares := make([]balance.Share, len(e.Shares))
		for j, s := range e.Shares {
			shares[j] = balance.Share{ExpenseID: e.ID, UserID: s.UserID, Paid: s.Paid, Owed: s.Owed}
		}
		out[i] = balance.Expense{ID: e.ID, IsPayment: e.IsPayment, Shares: shares}
	}
	return out
}

func nameMap(members []user.User) map[int64]string {
	m := make(map[int64]string, len(members))
	for _, u := range members {
		m[u.ID] = u.Name
	}
	return m
}

// List serves GET /groups.
func (h *Handlers) List(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	groups, err := h.Store.ForUser(u.ID)
	if err != nil {
		slog.Error("list groups", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	type groupRow struct {
		Group   Group
		YourNet int64
		Members int
	}
	var rows []groupRow
	for _, g := range groups {
		row := groupRow{Group: g}
		if brows, err := h.Expenses.BalanceRows(g.ID); err == nil {
			seen := map[int64]bool{}
			for _, br := range brows {
				if br.UserID == u.ID {
					row.YourNet += br.Paid - br.Owed
				}
				if !seen[br.UserID] {
					seen[br.UserID] = true
					row.Members++
				}
			}
		}
		rows = append(rows, row)
	}
	h.Render.Render(w, http.StatusOK, "groups.html", struct {
		render.Common
		Groups []groupRow
	}{Common: h.common(r, u), Groups: rows})
}

// Create serves POST /groups.
func (h *Handlers) Create(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		redirectFlash(w, r, "/groups", "Please give the group a name.", "error")
		return
	}
	g, err := h.Store.Create(name, KindGroup, u.ID)
	if err != nil {
		slog.Error("create group", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if err := h.Store.AddMember(g.ID, u.ID); err != nil {
		slog.Error("add creator membership", "error", err)
	}
	h.Activity.Append(g.ID, 0, u.ID, activity.GroupCreated, u.Name+" created the group "+g.Name)
	http.Redirect(w, r, fmt.Sprintf("/groups/%d", g.ID), http.StatusSeeOther)
}

// Detail serves GET /groups/{id}.
func (h *Handlers) Detail(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	g, err := h.Store.ByID(id)
	if errors.Is(err, ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		slog.Error("load group", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	ok, err := h.Store.IsMember(g.ID, u.ID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if !ok {
		h.notFound(w, r)
		return
	}

	members, err := h.Store.Members(g.ID)
	if err != nil {
		slog.Error("group members", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	expenses, err := h.Expenses.ListForGroup(g.ID)
	if err != nil {
		slog.Error("group expenses", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	bexp := BalanceExpenses(expenses)
	net := balance.Net(bexp)
	names := nameMap(members)

	page := groupPage{Common: h.common(r, u), Group: g}
	page.TotalSpend = 0
	for _, e := range expenses {
		if !e.IsPayment {
			page.TotalSpend += e.Total()
		}
	}

	for _, m := range members {
		row := memberRow{User: m, Net: net[m.ID], IsYou: m.ID == u.ID}
		// Pairwise position between the current user and this member.
		for _, d := range balance.Pairwise(bexp) {
			if d.From == u.ID && d.To == m.ID {
				row.YouOwe = d.Amount
			}
			if d.From == m.ID && d.To == u.ID {
				row.OwesYou = d.Amount
			}
		}
		page.Members = append(page.Members, row)
	}

	for _, d := range balance.Simplify(bexp) {
		page.Debts = append(page.Debts, debtView{
			FromName: names[d.From], ToName: names[d.To], Amount: d.Amount,
		})
	}

	for _, e := range expenses {
		er := expenseRow{Expense: e}
		var payerNames []string
		for _, s := range e.Shares {
			if s.Paid > 0 {
				payerNames = append(payerNames, names[s.UserID])
			}
			if s.UserID == u.ID {
				er.YourPaid = s.Paid
				er.YourOwed = s.Owed
			}
		}
		er.PaidBy = strings.Join(payerNames, ", ")
		er.YourImpact = er.YourPaid - er.YourOwed
		page.Expenses = append(page.Expenses, er)
	}

	page.Pending, err = h.Invites.PendingForGroup(g.ID)
	if err != nil {
		slog.Error("pending invites", "error", err)
	}
	page.Activity, err = h.Activity.ForGroup(g.ID, 30)
	if err != nil {
		slog.Error("group activity", "error", err)
	}
	page.CanSettle = len(page.Members) > 1
	// Only surface drafts that hold something: a blank draft is just the
	// remnant of an aborted "Add expense" click.
	if d, found, err := h.Expenses.FindDraft(u.ID, g.ID); err != nil {
		slog.Error("find draft", "error", err)
	} else if found && d.HasContent() {
		page.DraftID = d.ID
	}

	h.Render.Render(w, http.StatusOK, "group.html", page)
}

// AddMember serves POST /groups/{id}/members. Three cases: an email that
// belongs to an existing account joins straight away; an unknown email gets
// an invite link; anything else is treated as a name-only guest member.
func (h *Handlers) AddMember(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	g, err := h.Store.ByID(id)
	if err != nil || g.Kind != KindGroup {
		h.notFound(w, r)
		return
	}
	if ok, _ := h.Store.IsMember(g.ID, u.ID); !ok {
		h.notFound(w, r)
		return
	}

	ident := strings.TrimSpace(r.PostFormValue("identity"))
	if ident == "" {
		redirectFlash(w, r, groupURL(g.ID), "Enter a name or email.", "error")
		return
	}

	back := groupURL(g.ID)
	if strings.Contains(ident, "@") {
		if existing, err := h.Users.ByEmail(ident); err == nil && !existing.IsGuest {
			if err := h.Store.AddMember(g.ID, existing.ID); err != nil {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			h.Activity.Append(g.ID, 0, u.ID, activity.MemberAdded, existing.Name+" was added to "+g.Name)
			redirectFlash(w, r, back, existing.Name+" has been added to the group.", "ok")
			return
		}
		token, err := h.Invites.Create(strings.ToLower(ident), g.ID, u.ID)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		link := baseURL(r) + "/invite/" + token
		h.Activity.Append(g.ID, 0, u.ID, activity.MemberAdded, "An invite was sent to "+ident+" for "+g.Name)
		redirectFlash(w, r, back, "Invite created — send this link: "+link, "ok")
		return
	}

	// Name-only guest: reuse a guest with that name in this group, else create.
	if _, err := h.findOrCreateGuest(g.ID, ident, u.ID); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.Activity.Append(g.ID, 0, u.ID, activity.MemberAdded, ident+" was added to "+g.Name)
	redirectFlash(w, r, back, ident+" has been added to the group.", "ok")
}

// findOrCreateGuest adds a name-only member, avoiding duplicates per group.
func (h *Handlers) findOrCreateGuest(groupID int64, name string, invitedBy int64) (int64, error) {
	members, err := h.Store.Members(groupID)
	if err != nil {
		return 0, err
	}
	for _, m := range members {
		if m.IsGuest && strings.EqualFold(m.Name, name) {
			return m.ID, nil
		}
	}
	guest := user.User{Name: name, IsGuest: true,
		InvitedBy: sql.NullInt64{Int64: invitedBy, Valid: true}}
	if err := h.Users.Create(&guest); err != nil {
		return 0, err
	}
	return guest.ID, h.Store.AddMember(groupID, guest.ID)
}

// Settings serves POST /groups/{id}/settings.
func (h *Handlers) Settings(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	g, err := h.Store.ByID(id)
	if err != nil || g.Kind != KindGroup {
		h.notFound(w, r)
		return
	}
	if ok, _ := h.Store.IsMember(g.ID, u.ID); !ok {
		h.notFound(w, r)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = g.Name
	}
	if err := h.Store.Update(g.ID, name); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	redirectFlash(w, r, groupURL(g.ID), "Group settings saved.", "ok")
}

// Settle serves POST /groups/{id}/settle: records a payment expense from one
// member to another.
func (h *Handlers) Settle(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.notFound(w, r)
		return
	}
	g, err := h.Store.ByID(id)
	if err != nil {
		h.notFound(w, r)
		return
	}
	if ok, _ := h.Store.IsMember(g.ID, u.ID); !ok {
		h.notFound(w, r)
		return
	}

	fromID, err1 := strconv.ParseInt(r.PostFormValue("from"), 10, 64)
	toID, err2 := strconv.ParseInt(r.PostFormValue("to"), 10, 64)
	amount, err3 := money.Parse(r.PostFormValue("amount"))
	if err1 != nil || err2 != nil || err3 != nil || amount <= 0 {
		redirectFlash(w, r, groupURL(g.ID), "Choose who paid, who received, and a positive amount.", "error")
		return
	}
	if fromID == toID {
		redirectFlash(w, r, groupURL(g.ID), "The payer and the recipient must be different people.", "error")
		return
	}
	if ok, _ := h.Store.IsMember(g.ID, fromID); !ok {
		redirectFlash(w, r, groupURL(g.ID), "The payer must be a group member.", "error")
		return
	}
	if ok, _ := h.Store.IsMember(g.ID, toID); !ok {
		redirectFlash(w, r, groupURL(g.ID), "The recipient must be a group member.", "error")
		return
	}

	members, _ := h.Store.Members(g.ID)
	names := nameMap(members)
	e := &expense.Expense{
		GroupID:     g.ID,
		Description: fmt.Sprintf("Payment from %s to %s", names[fromID], names[toID]),
		Category:    "payment",
		Date:        time.Now().Format("2006-01-02"),
		IsPayment:   true,
		SplitMode:   expense.SplitEven,
		CreatedBy:   u.ID,
		Shares: []expense.Share{
			{UserID: fromID, Paid: amount},
			{UserID: toID, Owed: amount},
		},
	}
	if err := h.Expenses.Insert(e); err != nil {
		slog.Error("insert payment", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.Activity.Append(g.ID, e.ID, u.ID, activity.PaymentAdded,
		fmt.Sprintf("%s paid %s %s", names[fromID], names[toID], money.Format(amount, h.common(r, u).Symbol)))
	redirectFlash(w, r, groupURL(g.ID), "Payment recorded.", "ok")
}

func (h *Handlers) notFound(w http.ResponseWriter, r *http.Request) {
	h.Render.Render(w, http.StatusNotFound, "error.html", struct {
		render.Common
		Message string
	}{Common: h.Render.CommonFrom(r, nil), Message: "That page doesn't exist, or you don't have access to it."})
}

func groupURL(id int64) string { return fmt.Sprintf("/groups/%d", id) }

// baseURL reconstructs the scheme+host for invite links.
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

// redirectFlash is duplicated from auth to avoid an import direction problem.
func redirectFlash(w http.ResponseWriter, r *http.Request, path, flash, kind string) {
	http.Redirect(w, r, path+"?flash="+url.QueryEscape(flash)+"&flash-kind="+kind, http.StatusSeeOther)
}
