package expense

// Page handlers for draft expenses: the add-expense flow is draft-backed so
// a refresh or process kill on mobile never loses entered data.
//
//	GET  /groups/{id}/expenses/new  opens (creating) the draft, redirects
//	GET  /drafts/{id}               the form, pre-filled from the draft
//	PUT  /drafts/{id}               autosave from expense-form.js
//	POST /drafts/{id}/clear         blank the draft and start over
//	POST /drafts/{id}/discard       delete the draft (group page banner)
//	POST /drafts/{id}/submit        validate, insert the expense, drop draft

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/greboid/splitpayments/internal/activity"
	"github.com/greboid/splitpayments/internal/money"
)

// draftOr404 loads a draft the current user owns, in a group they still
// belong to; anything else renders a 404 (no existence leak).
func (h *Handlers) draftOr404(w http.ResponseWriter, r *http.Request, id string) (Draft, bool) {
	u := h.currentUser(r)
	d, err := h.Store.DraftByID(id)
	if errors.Is(err, ErrNotFound) {
		notFound(w, r, h.Render)
		return Draft{}, false
	}
	if err != nil {
		slog.Error("load draft", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return Draft{}, false
	}
	if d.UserID != u.ID {
		notFound(w, r, h.Render)
		return Draft{}, false
	}
	if ok, err := h.Store.IsMember(d.GroupID, u.ID); err != nil || !ok {
		notFound(w, r, h.Render)
		return Draft{}, false
	}
	return d, true
}

// New serves GET /groups/{id}/expenses/new: it opens the user's draft for
// the group (creating it on the first click) and redirects to the draft
// page, where the form autosaves as it is filled in.
func (h *Handlers) New(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	groupID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w, r, h.Render)
		return
	}
	if _, ok := h.memberOr404(w, r, groupID); !ok {
		return
	}
	d, err := h.Store.OpenDraft(u.ID, groupID)
	if err != nil {
		slog.Error("open draft", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/drafts/"+d.ID, http.StatusSeeOther)
}

// ViewDraft serves GET /drafts/{id}: the add-expense form, pre-filled from
// the autosaved draft.
func (h *Handlers) ViewDraft(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	d, ok := h.draftOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	members, ok := h.memberOr404(w, r, d.GroupID)
	if !ok {
		return
	}
	groupName, _ := h.Store.GroupName(d.GroupID)
	memberIDs := memberIDList(members)

	page := formPage{
		Common: h.common(r, u), GroupID: d.GroupID, GroupName: groupName,
		Members: members, Form: formFromDraft(d, memberIDs, u.ID),
		Drafting: true, DraftID: d.ID, SubmitLabel: "Add expense",
	}
	page.Payload = buildPayload(page.Form, members, page.Symbol)
	h.Render.Render(w, http.StatusOK, "expense-form.html", page)
}

// formFromDraft reconstructs the form from a draft's saved body. An empty or
// unreadable body means a blank form; the receipt name is re-validated so a
// tampered body can't point the preview anywhere.
func formFromDraft(d Draft, memberIDs []int64, currentUser int64) ExpenseForm {
	blank := func() ExpenseForm {
		return BlankForm(memberIDs, currentUser, time.Now().Format("2006-01-02"))
	}
	if strings.TrimSpace(d.FormBody) == "" {
		return blank()
	}
	values, err := url.ParseQuery(d.FormBody)
	if err != nil {
		return blank()
	}
	f := ParseExpenseForm(values, memberIDs)
	if !ValidReceiptFile(f.ReceiptFile) {
		f.ReceiptFile = ""
	}
	return f
}

// SaveDraftForm serves PUT /drafts/{id}: the form editor's autosave. The body
// is the same urlencoded field set the real submit posts; it is stored
// verbatim (amounts as typed) and only re-validated on render and submit.
func (h *Handlers) SaveDraftForm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.draftOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	h.saveDraftForm(d.ID, r.PostForm)
	w.WriteHeader(http.StatusNoContent)
}

// saveDraftForm persists a form snapshot, lifting the receipt file name out
// (validated) so receipt serving can authorise on the draft.
func (h *Handlers) saveDraftForm(draftID string, form url.Values) {
	receipt := strings.TrimSpace(form.Get("receipt_file"))
	if !ValidReceiptFile(receipt) {
		receipt = ""
	}
	if err := h.Store.SaveDraftBody(draftID, form.Encode(), receipt); err != nil {
		slog.Error("save draft", "error", err)
	}
}

// ClearDraft serves POST /drafts/{id}/clear: blank the draft and start
// over. The draft itself stays, so the same URL keeps working — this is the
// form page's button; the group page banner discards outright instead.
func (h *Handlers) ClearDraft(w http.ResponseWriter, r *http.Request) {
	d, ok := h.draftOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := h.Store.ClearDraft(d.ID); err != nil {
		slog.Error("clear draft", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/drafts/"+d.ID, http.StatusSeeOther)
}

// DiscardDraft serves POST /drafts/{id}/discard: the group page banner's
// Discard. Unlike clear, the draft row is deleted outright — the user is
// sent to the group page and nothing lingers to bring the banner back.
func (h *Handlers) DiscardDraft(w http.ResponseWriter, r *http.Request) {
	d, ok := h.draftOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if err := h.Store.DeleteDraft(d.ID); err != nil {
		slog.Error("discard draft", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/groups/%d", d.GroupID), http.StatusSeeOther)
}

// SubmitDraft serves POST /drafts/{id}/submit: validate the posted form and
// turn the draft into a real expense, then drop the draft. When validation
// fails, whatever was submitted stays in the draft, so nothing is lost.
func (h *Handlers) SubmitDraft(w http.ResponseWriter, r *http.Request) {
	u := h.currentUser(r)
	d, ok := h.draftOr404(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	members, ok := h.memberOr404(w, r, d.GroupID)
	if !ok {
		return
	}
	groupName, _ := h.Store.GroupName(d.GroupID)
	memberIDs := memberIDList(members)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := ParseExpenseForm(r.PostForm, memberIDs)
	e, errMsg := h.buildExpense(&form, memberIDs, d.GroupID, u.ID)
	if errMsg != "" {
		h.saveDraftForm(d.ID, r.PostForm)
		page := formPage{
			Common: h.common(r, u), GroupID: d.GroupID, GroupName: groupName,
			Members: members, Form: form, Drafting: true, DraftID: d.ID,
			Error: errMsg, SubmitLabel: "Add expense",
		}
		page.Payload = buildPayload(page.Form, members, page.Symbol)
		h.Render.Render(w, http.StatusUnprocessableEntity, "expense-form.html", page)
		return
	}
	if err := h.Store.Insert(e); err != nil {
		slog.Error("insert expense", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	h.Activity.Append(d.GroupID, e.ID, u.ID, activity.ExpenseAdded,
		fmt.Sprintf("%s added “%s” (%s)", u.Name, e.Description, money.Format(e.Total(), h.common(r, u).Symbol)))
	if err := h.Store.DeleteDraft(d.ID); err != nil {
		slog.Error("delete draft", "error", err)
	}
	http.Redirect(w, r, fmt.Sprintf("/groups/%d", d.GroupID), http.StatusSeeOther)
}
