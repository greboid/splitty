package expense

// Page handlers for creating, editing, viewing and deleting expenses.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/greboid/splitpayments/internal/activity"
	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/money"
	"github.com/greboid/splitpayments/internal/render"
	"github.com/greboid/splitpayments/internal/user"
)

type Handlers struct {
	Store    *Store
	Users    *user.Store
	Activity *activity.Store
	Render   *render.Renderer
}

// formPage is the template data for the new (draft), edit expense pages.
type formPage struct {
	render.Common
	GroupID     int64
	GroupName   string
	Members     []user.User
	Form        ExpenseForm
	Editing     bool
	ExpenseID   int64
	Drafting    bool   // new-expense flow: the form autosaves to a draft
	DraftID     string // set when Drafting
	Payload     string // JSON for the JS editor
	SubmitLabel string
	Error       string
}

// formPayload is the JSON contract with web/static/js/expense-form.js. When
// JS is active it takes over the payer/participant/item sections and
// initialises them from this payload; without JS the server-rendered even
// mode fallback in the template still submits a valid form.
type formPayload struct {
	Members      []payloadMember   `json:"members"`
	Symbol       string            `json:"symbol"`
	Mode         string            `json:"mode"`
	Payments     []payloadPair     `json:"payments"`
	Participants []int64           `json:"participants"`
	Exact        map[string]string `json:"exact"`
	Percent      map[string]string `json:"percent"`
	Shares       map[string]string `json:"shares"`
	Items        []payloadItem     `json:"items"`
}

type payloadMember struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type payloadPair struct {
	UserID int64  `json:"id"`
	Amount string `json:"amount"`
}

type payloadItem struct {
	Description string   `json:"description"`
	Amount      string   `json:"amount"`
	People      []string `json:"people"` // user ids as strings, or "all"
}

func buildPayload(f ExpenseForm, members []user.User, symbol string) string {
	p := formPayload{
		Symbol:  symbol,
		Mode:    f.Mode,
		Exact:   map[string]string{},
		Percent: map[string]string{},
		Shares:  map[string]string{},
	}
	if p.Mode == "" {
		p.Mode = SplitEven
	}
	for _, m := range members {
		p.Members = append(p.Members, payloadMember{ID: m.ID, Name: m.Name})
	}
	for _, payer := range f.Payers {
		p.Payments = append(p.Payments, payloadPair{UserID: payer.UserID, Amount: payer.Amount})
	}
	for id, on := range f.Participants {
		if on {
			p.Participants = append(p.Participants, id)
		}
	}
	sortInt64s(p.Participants)
	for id, v := range f.Exact {
		p.Exact[strconv.FormatInt(id, 10)] = v
	}
	for id, v := range f.Percent {
		p.Percent[strconv.FormatInt(id, 10)] = v
	}
	for id, v := range f.Shares {
		p.Shares[strconv.FormatInt(id, 10)] = v
	}
	for _, it := range f.Items {
		p.Items = append(p.Items, payloadItem{Description: it.Description, Amount: it.Amount, People: it.People})
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// notFound renders a friendly 404 for group/expense access violations.
func notFound(w http.ResponseWriter, r *http.Request, rp *render.Renderer) {
	rp.Render(w, http.StatusNotFound, "error.html", errorPageData{
		Common:  rp.CommonFrom(r, nil),
		Message: "That page doesn't exist, or you don't have access to it.",
	})
}

type errorPageData struct {
	render.Common
	Message string
}

func (h *Handlers) currentUser(r *http.Request) *user.User { return auth.UserFrom(r) }

func (h *Handlers) common(r *http.Request, u *user.User) render.Common {
	return h.Render.CommonFrom(r, &render.User{Name: u.Name, IsGuest: u.IsGuest})
}

// memberOr404 checks the user can see the group and returns its members.
func (h *Handlers) memberOr404(w http.ResponseWriter, r *http.Request, groupID int64) ([]user.User, bool) {
	u := h.currentUser(r)
	ok, err := h.Store.IsMember(groupID, u.ID)
	if err != nil {
		slog.Error("membership check", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return nil, false
	}
	if !ok {
		notFound(w, r, h.Render)
		return nil, false
	}
	members, err := h.Store.MemberList(groupID)
	if err != nil {
		slog.Error("member list", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return nil, false
	}
	return members, true
}

// buildExpense validates the form and assembles a share-complete Expense.
func (h *Handlers) buildExpense(form *ExpenseForm, memberIDs []int64, groupID, createdBy int64) (*Expense, string) {
	in, msg := form.ToSplitInput(memberIDs)
	if msg != "" {
		return nil, msg
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		return nil, err.Error()
	}
	e := &Expense{
		GroupID:     groupID,
		Description: form.Description,
		Notes:       form.Notes,
		Category:    form.Category,
		Date:        form.Date,
		SplitMode:   form.Mode,
		ReceiptFile: form.ReceiptFile,
		CreatedBy:   createdBy,
	}
	if !ValidReceiptFile(e.ReceiptFile) {
		e.ReceiptFile = ""
	}

	// One share row per participant carrying both sides: a payer who is
	// also an ower must keep their owed amount (Σ paid == Σ owed).
	shares := map[int64]*Share{}
	shareFor := func(uid int64) *Share {
		sh, ok := shares[uid]
		if !ok {
			sh = &Share{UserID: uid}
			shares[uid] = sh
		}
		return sh
	}
	for _, p := range in.Payers {
		shareFor(p.UserID).Paid = p.Amount
	}
	if form.Mode == SplitEven {
		// Keep zero-owed participants so an edit round-trip is faithful.
		for _, uid := range in.Participants {
			shareFor(uid).Owed = owed[uid]
		}
	} else {
		for uid := range owed {
			shareFor(uid).Owed = owed[uid]
		}
	}
	ids := make([]int64, 0, len(shares))
	for uid := range shares {
		ids = append(ids, uid)
	}
	sortInt64s(ids)
	for _, uid := range ids {
		e.Shares = append(e.Shares, *shares[uid])
	}

	for _, it := range form.Items {
		assignees, err := it.assignees(memberIDs)
		if err != nil {
			return nil, err.Error()
		}
		e.Items = append(e.Items, Item{Description: it.Description, Amount: it.amountValue(), Assignees: assignees})
	}
	return e, ""
}

// Edit serves GET/POST /expenses/{id}/edit.
func (h *Handlers) Edit(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w, r, h.Render)
		return
	}
	e, err := h.Store.ByID(id)
	if errors.Is(err, ErrNotFound) {
		notFound(w, r, h.Render)
		return
	}
	if err != nil {
		slog.Error("load expense", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	members, ok := h.memberOr404(w, r, e.GroupID)
	if !ok {
		return
	}
	groupName, _ := h.Store.GroupName(e.GroupID)
	memberIDs := memberIDList(members)

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		form := ParseExpenseForm(r.PostForm, memberIDs)
		updated, errMsg := h.buildExpense(&form, memberIDs, e.GroupID, e.CreatedBy)
		if errMsg == "" {
			updated.ID = e.ID
			updated.IsPayment = e.IsPayment
			if err := h.Store.Update(updated); err != nil {
				slog.Error("update expense", "error", err)
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			h.Activity.Append(e.GroupID, e.ID, u.ID, activity.ExpenseUpdated,
				fmt.Sprintf("%s updated “%s” (%s)", u.Name, updated.Description, money.Format(updated.Total(), h.common(r, u).Symbol)))
			http.Redirect(w, r, fmt.Sprintf("/expenses/%d", e.ID), http.StatusSeeOther)
			return
		}
		page := formPage{
			Common: h.common(r, u), GroupID: e.GroupID, GroupName: groupName,
			Members: members, Form: form, Editing: true, ExpenseID: e.ID,
			Error: errMsg, SubmitLabel: "Save changes",
		}
		page.Payload = buildPayload(page.Form, members, page.Symbol)
		h.Render.Render(w, http.StatusUnprocessableEntity, "expense-form.html", page)
		return
	}

	form := formFromExpense(e, memberIDs)
	page := formPage{
		Common: h.common(r, u), GroupID: e.GroupID, GroupName: groupName,
		Members: members, Form: form, Editing: true, ExpenseID: e.ID, SubmitLabel: "Save changes",
	}
	page.Payload = buildPayload(page.Form, members, page.Symbol)
	h.Render.Render(w, http.StatusOK, "expense-form.html", page)
}

// formFromExpense reconstructs an editable form from a stored expense.
func formFromExpense(e Expense, memberIDs []int64) ExpenseForm {
	f := ExpenseForm{
		Description:  e.Description,
		Notes:        e.Notes,
		Category:     e.Category,
		Date:         e.Date,
		Mode:         e.SplitMode,
		ReceiptFile:  e.ReceiptFile,
		Participants: map[int64]bool{},
		Exact:        map[int64]string{},
		Percent:      map[int64]string{},
		Shares:       map[int64]string{},
	}
	if f.Mode == "" {
		f.Mode = SplitExact
	}
	for _, s := range e.Shares {
		if s.Paid > 0 {
			f.Payers = append(f.Payers, FormPayer{UserID: s.UserID, Amount: formatAmount(s.Paid)})
		}
		if e.SplitMode == SplitEven {
			f.Participants[s.UserID] = true
		} else if s.Owed > 0 {
			f.Exact[s.UserID] = formatAmount(s.Owed)
		}
	}
	for _, it := range e.Items {
		fi := FormItem{Description: it.Description, Amount: formatAmount(it.Amount)}
		if len(memberIDs) > 0 && len(it.Assignees) == len(memberIDs) {
			fi.People = []string{"all"}
		} else {
			for _, a := range it.Assignees {
				fi.People = append(fi.People, strconv.FormatInt(a, 10))
			}
		}
		f.Items = append(f.Items, fi)
	}
	return f
}

func formatAmount(v int64) string {
	if v == 0 {
		return ""
	}
	return money.Format(v, "")
}

// Detail serves GET /expenses/{id}.
func (h *Handlers) Detail(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w, r, h.Render)
		return
	}
	e, err := h.Store.ByID(id)
	if errors.Is(err, ErrNotFound) {
		notFound(w, r, h.Render)
		return
	}
	if err != nil {
		slog.Error("load expense", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	members, ok := h.memberOr404(w, r, e.GroupID)
	if !ok {
		return
	}
	names := map[int64]string{}
	for _, m := range members {
		names[m.ID] = m.Name
	}
	groupName, _ := h.Store.GroupName(e.GroupID)

	type row struct {
		Name string
		Paid int64
		Owed int64
	}
	var rows []row
	for _, s := range e.Shares {
		rows = append(rows, row{Name: names[s.UserID], Paid: s.Paid, Owed: s.Owed})
	}
	type itemRow struct {
		Description string
		Amount      int64
		People      []string
	}
	var items []itemRow
	for _, it := range e.Items {
		ir := itemRow{Description: it.Description, Amount: it.Amount}
		for _, a := range it.Assignees {
			ir.People = append(ir.People, names[a])
		}
		items = append(items, ir)
	}

	h.Render.Render(w, http.StatusOK, "expense-detail.html", struct {
		render.Common
		Expense    Expense
		GroupName  string
		Rows       []row
		Items      []itemRow
		ReceiptURL string
	}{
		Common:     h.common(r, u),
		Expense:    e,
		GroupName:  groupName,
		Rows:       rows,
		Items:      items,
		ReceiptURL: receiptURL(e.ReceiptFile),
	})
}

func receiptURL(file string) string {
	if file == "" {
		return ""
	}
	return "/receipts/" + file
}

// Delete serves POST /expenses/{id}/delete.
func (h *Handlers) Delete(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w, r, h.Render)
		return
	}
	e, err := h.Store.ByID(id)
	if errors.Is(err, ErrNotFound) {
		notFound(w, r, h.Render)
		return
	}
	if err != nil {
		slog.Error("load expense", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if _, ok := h.memberOr404(w, r, e.GroupID); !ok {
		return
	}
	if err := h.Store.SoftDelete(e.ID); err != nil {
		slog.Error("delete expense", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.Activity.Append(e.GroupID, e.ID, u.ID, activity.ExpenseDeleted,
		fmt.Sprintf("%s deleted “%s”", u.Name, e.Description))
	http.Redirect(w, r, fmt.Sprintf("/groups/%d", e.GroupID), http.StatusSeeOther)
}

func memberIDList(members []user.User) []int64 {
	out := make([]int64, len(members))
	for i, m := range members {
		out[i] = m.ID
	}
	return out
}
