package server

// End-to-end tests against a real SQLite database (in-memory) and the full
// middleware stack, covering: setup → group → itemized expense → simplify →
// settle-up, plus invite claiming and authorisation.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/greboid/splitpayments/internal/activity"
	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/config"
	"github.com/greboid/splitpayments/internal/expense"
	"github.com/greboid/splitpayments/internal/group"
	"github.com/greboid/splitpayments/internal/receipt"
	"github.com/greboid/splitpayments/internal/render"
	"github.com/greboid/splitpayments/internal/testdb"
	"github.com/greboid/splitpayments/internal/user"
	"github.com/greboid/splitpayments/web"
)

type app struct {
	t        *testing.T
	srv      *httptest.Server
	client   *http.Client
	db       *sql.DB
	sessions *auth.SessionStore
	receipts *receipt.Store
}

func newApp(t *testing.T) *app {
	t.Helper()
	db := testdb.Open(t)

	r, err := render.New(web.Templates(), "GBP", "£", false, web.AssetVersion())
	if err != nil {
		t.Fatal(err)
	}
	users := user.NewStore(db)
	groups := group.NewStore(db)
	expenses := expense.NewStore(db)
	activityStore := activity.NewStore(db)
	sessions := &auth.SessionStore{DB: db, Lifetime: time.Hour, Secure: false}
	invites := &auth.InviteStore{DB: db, Lifetime: time.Hour}
	receipts := receipt.NewStore(db)
	s := &Server{
		Cfg:      config.Config{Currency: "GBP", CurrencySymbol: "£"},
		Users:    users,
		Sessions: sessions,
		Invites:  invites,
		AuthH: &auth.Handlers{Users: users, Sessions: sessions, Invites: invites,
			Groups: groups, Activity: activityStore, Render: r},
		Groups:   groups,
		GroupH:   &group.Handlers{Store: groups, Users: users, Expenses: expenses, Activity: activityStore, Invites: invites, Render: r},
		Expenses: expenses,
		ExpenseH: &expense.Handlers{Store: expenses, Users: users, Activity: activityStore, Render: r},
		Activity: activityStore,
		// A Client (pointing nowhere) turns the upload endpoint on; only
		// Analyze talks to it, which no test reaches.
		ReceiptH: &receipt.Handlers{Store: receipts, Expenses: expenses,
			Client: receipt.NewClient("http://127.0.0.1:1", "test", "test", receipt.FormatOpenAI)},
		ActivityH: &activity.Handler{Store: activityStore, Render: r},
		Render:    r,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	return &app{t: t, srv: ts, client: &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}, db: db, sessions: sessions, receipts: receipts}
}

func (a *app) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func (a *app) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := a.client.Get(a.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (a *app) postOK(t *testing.T, path, body string) *http.Response {
	t.Helper()
	resp := a.post(t, path, body)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s: status %d, want 303", path, resp.StatusCode)
	}
	return resp
}

func (a *app) put(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, a.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// openDraft clicks "Add expense": GET /groups/{id}/expenses/new and return
// the redirected draft path.
func (a *app) openDraft(t *testing.T, groupID int64) string {
	t.Helper()
	resp := a.get2(t, fmt.Sprintf("/groups/%d/expenses/new", groupID))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("open draft: %d, want 303", resp.StatusCode)
	}
	loc, _ := resp.Location()
	return loc.Path
}

// createUser plants a bare user row, for a second identity in one app.
func (a *app) createUser(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	if err := a.db.QueryRow(`INSERT INTO users (name) VALUES (?) RETURNING id`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// loginAs plants a session for the given user, switching the test client's
// identity within one app.
func (a *app) loginAs(t *testing.T, userID int64) {
	t.Helper()
	token, err := a.sessions.Create(userID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(a.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	a.client.Jar.SetCookies(u, []*http.Cookie{{Name: "sp_session", Value: token, Path: "/"}})
}

// uploadReceipt POSTs a small image to /receipts the way scan.js does —
// PNG magic so the content type passes, attached to the given draft — and
// returns the stored file name.
func (a *app) uploadReceipt(t *testing.T, draftID string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("image", "receipt.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("\x89PNG\r\n\x1a\nnot-really-an-image")); err != nil {
		t.Fatal(err)
	}
	if draftID != "" {
		if err := mw.WriteField("draft", draftID); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/receipts", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("receipt upload: %d %s", resp.StatusCode, body)
	}
	var out struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("receipt upload response: %v (%s)", err, body)
	}
	return out.File
}

func (a *app) setupUser(t *testing.T, name, email, password string) {
	t.Helper()
	resp := a.postOK(t, "/setup", fmt.Sprintf("name=%s&email=%s&password=%s",
		url.QueryEscape(name), url.QueryEscape(email), password))
	if loc, _ := resp.Location(); loc.Path != "/" {
		t.Fatalf("setup redirected to %s", loc)
	}
}

func TestSetupLoginLogout(t *testing.T) {
	a := newApp(t)

	// A fresh instance redirects every page to setup; health checks stay
	// reachable.
	for _, path := range []string{"/", "/login", "/groups"} {
		resp := a.get2(t, path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("fresh instance %s: %d, want redirect", path, resp.StatusCode)
		}
		if loc, _ := resp.Location(); loc.Path != "/setup" {
			t.Fatalf("fresh instance %s redirected to %s, want /setup", path, loc.Path)
		}
	}
	if code, _ := a.get(t, "/healthz"); code != 200 {
		t.Fatalf("healthz on fresh instance: %d", code)
	}

	// Setup is first-run only.
	code, body := a.get(t, "/setup")
	if code != 200 || !strings.Contains(body, "create the first account") {
		t.Fatalf("setup page: %d", code)
	}
	a.setupUser(t, "Alice", "alice@example.com", "password123")

	// A second setup attempt bounces to login.
	if resp := a.post(t, "/setup", "name=X&email=x@x.com&password=password123"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second setup: %d, want redirect", resp.StatusCode)
	}

	// Dashboard works; logout; then everything requires login again.
	if code, _ := a.get(t, "/"); code != 200 {
		t.Fatalf("dashboard: %d", code)
	}
	a.postOK(t, "/logout", "")
	if resp := a.get2(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("dashboard after logout: %d", resp.StatusCode)
	}

	// Login again with wrong and right passwords.
	if resp := a.post(t, "/login", "email=alice@example.com&password=wrongwrong"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad login: %d", resp.StatusCode)
	}
	a.postOK(t, "/login", "email=alice@example.com&password=password123")
	if code, _ := a.get(t, "/"); code != 200 {
		t.Fatalf("dashboard after login: %d", code)
	}
}

func (a *app) get2(t *testing.T, path string) *http.Response {
	t.Helper()
	resp, err := a.client.Get(a.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestGroupExpenseSimplifySettle(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")

	a.postOK(t, "/groups", "name=Flat")
	if code, _ := a.get(t, "/groups/1"); code != 200 {
		t.Fatalf("group page: %d", code)
	}

	// Add two guests.
	a.postOK(t, "/groups/1/members", "identity=Bob")
	a.postOK(t, "/groups/1/members", "identity=Carol")

	// Itemized expense: Alice pays 46.00; pizza 20 shared by all, salad 20
	// Alice only, drinks 2 by Bob+Carol, 4.00 tax as an item shared by all.
	draft := a.openDraft(t, 1)
	a.postOK(t, draft+"/submit", strings.Join([]string{
		"description=Pizza+night", "date=2026-08-30", "category=dining",
		"split_mode=itemized", "payer_id_0=1", "payer_amount_0=46.00",
		"item_desc_0=Large+pizza", "item_amount_0=20.00", "item_people_0=all",
		"item_desc_1=Salad", "item_amount_1=20.00", "item_people_1=1",
		"item_desc_2=Drinks", "item_amount_2=2.00", "item_people_2=2", "item_people_2=3",
		"item_desc_3=Tax", "item_amount_3=4.00", "item_people_3=all",
	}, "&"))

	// Exact-mode expense paid by Bob: Bob pays 20, owes Alice 12.50, Carol 7.50.
	draft = a.openDraft(t, 1)
	a.postOK(t, draft+"/submit", "description=Taxi&date=2026-08-30&split_mode=exact&payer_id_0=2&payer_amount_0=20.00&exact_1=12.50&exact_2=7.50")

	// Payments are always simplified; settle the single suggested debt.
	a.postOK(t, "/groups/1/settings", "name=Flat")
	_, grpBody := a.get(t, "/groups/1")
	if !strings.Contains(grpBody, "Suggested payments") {
		t.Error("group page should show suggested payments")
	}
	if !strings.Contains(grpBody, "Pizza night") {
		t.Error("group page should list the itemized expense")
	}

	// A payment expense reverses part of Bob's position: Bob pays Alice 10.
	a.postOK(t, "/groups/1/settle", "from=2&to=1&amount=10.00")
	_, grpBody = a.get(t, "/groups/1")
	if !strings.Contains(grpBody, "Payment from Bob to Alice") {
		t.Error("payment should appear in the expense list")
	}

	// Delete the taxi expense; balances must move again.
	a.postOK(t, "/expenses/2/delete", "")
	_, grpBody = a.get(t, "/groups/1")
	if strings.Contains(grpBody, ">Taxi<") {
		t.Error("deleted expense should be hidden")
	}

	// CSV export lists the header pair per member.
	code, csvBody := a.get(t, "/groups/1/export.csv")
	if code != 200 || !strings.Contains(csvBody, "Alice paid,Alice owed") {
		t.Fatalf("csv export: %d %q", code, csvBody[:80])
	}

	// Activity feed has entries for every action.
	_, actBody := a.get(t, "/activity")
	for _, want := range []string{"created the group", "added “Pizza night”", "paid"} {
		if !strings.Contains(actBody, want) {
			t.Errorf("activity feed missing %q", want)
		}
	}
}

func TestSettleRejectsSelfPayment(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Flat")
	a.postOK(t, "/groups/1/members", "identity=Bob")

	resp := a.post(t, "/groups/1/settle", "from=1&to=1&amount=10.00")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("self settle: status %d, want 303", resp.StatusCode)
	}
	loc, _ := resp.Location()
	if !strings.Contains(loc.Query().Get("flash"), "must be different") {
		t.Fatalf("self settle flash: %q", loc.Query().Get("flash"))
	}

	// The rejected payment must not have been recorded.
	_, body := a.get(t, "/groups/1")
	if strings.Contains(body, "Payment from") {
		t.Error("self payment should not be recorded")
	}
}

func TestInviteFlow(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Trip")
	a.postOK(t, "/groups/1/members", "identity=frank@example.com")

	// Fetch the pending token via the flash-link test helper below.
	token := inviteTokenFromGroup(t, a)

	// Anonymous visitor sees the claim form and becomes a member.
	code, claimPage := a.get(t, "/invite/"+token)
	if code != 200 || !strings.Contains(claimPage, "Claim your account") {
		t.Fatalf("invite page: %d", code)
	}
	a.postOK(t, "/invite/"+token, "name=Frank&password=frankpass1")

	if code, _ := a.get(t, "/groups/1"); code != 200 {
		t.Fatalf("claimed invite should be a member: %d", code)
	}

	// The invite is consumed.
	_, again := a.get(t, "/invite/"+token)
	if !strings.Contains(again, "already been used") {
		t.Error("invite should be marked accepted")
	}
}

// inviteTokenFromGroup re-posts the member add and pulls the token from the
// flash redirect (the only place the raw token is exposed).
func inviteTokenFromGroup(t *testing.T, a *app) string {
	t.Helper()
	resp := a.postOK(t, "/groups/1/members", "identity=frank2@example.com")
	loc, _ := resp.Location()
	flash := loc.Query().Get("flash")
	idx := strings.Index(flash, "/invite/")
	if idx < 0 {
		return ""
	}
	return strings.TrimPrefix(flash[idx:], "/invite/")
}

// The flash contains the raw token; fetch it from the redirect target.
func TestInviteLinkInFlash(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Trip")
	resp := a.postOK(t, "/groups/1/members", "identity=grace@example.com")
	loc, _ := resp.Location()
	raw, _ := url.Parse(loc.String())
	flash := raw.Query().Get("flash")
	idx := strings.Index(flash, "/invite/")
	if idx < 0 {
		t.Fatalf("flash %q has no invite link", flash)
	}
	token := strings.TrimPrefix(flash[idx:], "/invite/")

	// Third user cannot see Alice's group (404: no existence leak).
	other := newApp(t)
	other.setupUser(t, "Mallory", "mallory@example.com", "password123")
	if resp := other.get2(t, "/groups/1"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider should get 404, got %d", resp.StatusCode)
	}

	a.postOK(t, "/invite/"+token, "name=Grace&password=gracepass")
	if code, _ := a.get(t, "/groups/1"); code != 200 {
		t.Fatalf("grace joined: %d", code)
	}
}

func TestGuestUpgradeViaInvite(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Flat")
	a.postOK(t, "/groups/1/members", "identity=Dora") // guest

	// Invite the guest's email... guests have no email; invite a fresh one,
	// and confirm a guest can't be found by it.
	resp := a.postOK(t, "/groups/1/members", "identity=dora@example.com")
	loc, _ := resp.Location()
	flash := loc.Query().Get("flash")
	idx := strings.Index(flash, "/invite/")
	if idx < 0 {
		t.Fatalf("expected invite link in flash %q", flash)
	}
	token := strings.TrimPrefix(flash[idx:], "/invite/")
	a.postOK(t, "/invite/"+token, "name=Dora Smith&password=dorapass1")

	// The claim creates a full account joined to the group; the name-only
	// guest stays (nothing links them, so no merge is attempted).
	var isGuest bool
	var count int
	if err := a.db.QueryRow(`SELECT is_guest FROM users WHERE name = 'Dora Smith'`).Scan(&isGuest); err != nil {
		t.Fatal(err)
	}
	if isGuest {
		t.Error("claimed account should no longer be a guest")
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM memberships WHERE group_id = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("group should have 3 members, has %d", count)
	}
}

// Inviting an external (not yet registered) email from the friends page:
// it creates an invited placeholder friend and hands back a claim link;
// claiming upgrades the placeholder in place, keeping the direct ledger.
func TestFriendInviteFlow(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")

	// Your own email is rejected outright.
	resp := a.postOK(t, "/friends", "identity=alice@example.com")
	if loc, _ := resp.Location(); !strings.Contains(loc.Query().Get("flash"), "own email") {
		t.Fatalf("self invite flash: %q", loc.Query().Get("flash"))
	}

	// An unknown email becomes an invited friend with a shareable link.
	resp = a.postOK(t, "/friends", "identity=Frank@Example.com")
	loc, _ := resp.Location()
	flash := loc.Query().Get("flash")
	idx := strings.Index(flash, "/invite/")
	if idx < 0 {
		t.Fatalf("expected invite link in flash %q", flash)
	}
	token := strings.TrimPrefix(flash[idx:], "/invite/")

	_, friendsBody := a.get(t, "/friends")
	if !strings.Contains(friendsBody, "frank@example.com") || !strings.Contains(friendsBody, "invited") {
		t.Error("friends page should list the placeholder as invited")
	}

	// Re-adding the same email re-issues a link without duplicating them.
	if resp := a.postOK(t, "/friends", "identity=frank@example.com"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("re-invite: %d", resp.StatusCode)
	}
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE email = 'frank@example.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("re-invite created %d users, want 1", n)
	}

	// The claim form prefills the email's local part as the name and the
	// claim upgrades the placeholder in place.
	code, claimPage := a.get(t, "/invite/"+token)
	if code != 200 || !strings.Contains(claimPage, "Claim your account") || !strings.Contains(claimPage, `value="frank"`) {
		t.Fatalf("invite page: %d", code)
	}
	a.postOK(t, "/invite/"+token, "name=Frank&password=frankpass1")

	var isGuest bool
	var frankID int64
	if err := a.db.QueryRow(`SELECT is_guest, id FROM users WHERE email = 'frank@example.com'`).Scan(&isGuest, &frankID); err != nil {
		t.Fatal(err)
	}
	if isGuest {
		t.Error("claimed friend should be a full account")
	}

	// Frank now has an account with Alice as his friend.
	a.loginAs(t, frankID)
	code, body := a.get(t, "/friends")
	if code != 200 || !strings.Contains(body, "Alice") {
		t.Errorf("new user should see Alice as a friend: %d", code)
	}

	// And Alice sees Frank as a full account, no longer marked invited.
	a.loginAs(t, 1)
	_, friendsBody = a.get(t, "/friends")
	if strings.Contains(friendsBody, "invited") {
		t.Error("claimed friend should no longer be marked invited")
	}
}

func TestReceiptServingRequiresMembership(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	if code, _ := a.get(t, "/receipts/00000000-0000-4000-8000-000000000000.png"); code != 404 {
		t.Fatalf("unknown receipt: %d", code)
	}
}

// A receipt that has just been scanned but is not attached to an expense
// yet — the window the scan preview needs — is served to any signed-in
// user; the file name is an unguessable random UUID.
func TestUnattachedReceiptServedToSignedInUser(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	file, err := a.receipts.Save("image/png", bytes.NewReader([]byte("fresh-scan-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if code, img := a.get(t, "/receipts/"+file); code != 200 || img != "fresh-scan-bytes" {
		t.Fatalf("unattached receipt: %d %q", code, img)
	}
}

// A scanned receipt attached to an expense must survive saving and stay
// visible: the detail page shows the photo, members can fetch the image,
// and an edit round-trip keeps it attached.
func TestReceiptSurvivesSaveAndEdit(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Flat")

	file, err := a.receipts.Save("image/png", bytes.NewReader([]byte("fake-png-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	// Itemized, so the detail page renders its Items section too — the
	// render previously aborted before reaching it.
	form := func() string {
		return strings.Join([]string{
			"description=Coffee", "date=2026-08-31", "split_mode=itemized",
			"payer_id_0=1", "payer_amount_0=4.50",
			"item_desc_0=Flat+white", "item_amount_0=4.50", "item_people_0=all",
			"receipt_file=" + file,
		}, "&")
	}
	draft := a.openDraft(t, 1)
	a.postOK(t, draft+"/submit", form())

	// The detail page shows the photo.
	code, body := a.get(t, "/expenses/1")
	if code != 200 || !strings.Contains(body, `src="/receipts/`+file+`"`) {
		t.Fatalf("expense detail should show the receipt: %d", code)
	}
	if !strings.Contains(body, "Flat white") {
		t.Fatal("expense detail should list the items")
	}

	// Group members can fetch the image itself.
	code, img := a.get(t, "/receipts/"+file)
	if code != 200 || img != "fake-png-bytes" {
		t.Fatalf("receipt serve: %d %q", code, img)
	}

	// The edit form carries the attachment and keeps it after a save.
	_, editBody := a.get(t, "/expenses/1/edit")
	if !strings.Contains(editBody, `value="`+file+`"`) {
		t.Fatal("edit form should carry the receipt file")
	}
	a.postOK(t, "/expenses/1/edit", form())
	code, body = a.get(t, "/expenses/1")
	if code != 200 || !strings.Contains(body, `src="/receipts/`+file+`"`) {
		t.Fatalf("receipt should survive an edit: %d", code)
	}
}

// The draft flow: clicking Add expense creates a draft, entered data
// survives a reload via the autosave, the top button blanks it, and
// submitting turns it into a real expense and drops it.
func TestDraftExpenseFlow(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Flat")
	a.postOK(t, "/groups/1/members", "identity=Bob")

	// Clicking Add expense creates a draft; clicking again reopens the same.
	draft := a.openDraft(t, 1)
	if !strings.HasPrefix(draft, "/drafts/") {
		t.Fatalf("expected a /drafts/ path, got %q", draft)
	}
	if again := a.openDraft(t, 1); again != draft {
		t.Fatalf("second click opened %q, want %q", again, draft)
	}

	// The draft page renders the form with a clear-draft button.
	code, body := a.get(t, draft)
	if code != 200 || !strings.Contains(body, "Clear draft") || !strings.Contains(body, `action="`+draft+`/submit"`) {
		t.Fatalf("draft page: %d", code)
	}

	// An untouched draft is not offered on the group page.
	if _, groupBody := a.get(t, "/groups/1"); strings.Contains(groupBody, "unfinished expense") {
		t.Error("group page should not offer a blank draft")
	}

	// The autosave persists partially-entered data.
	if resp := a.put(t, draft, "description=Taxi+draft&date=2026-08-30&split_mode=even&payer_id_0=1&payer_amount_0=10.00"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("autosave: %d, want 204", resp.StatusCode)
	}
	_, body = a.get(t, draft)
	if !strings.Contains(body, `value="Taxi draft"`) {
		t.Error("reloaded draft should show the autosaved description")
	}

	// The group page offers to resume the unfinished draft.
	_, groupBody := a.get(t, "/groups/1")
	for _, want := range []string{"unfinished expense", "Resume draft", draft} {
		if !strings.Contains(groupBody, want) {
			t.Errorf("group page missing %q", want)
		}
	}

	// Clear blanks the draft, and the banner goes away with it.
	a.postOK(t, draft+"/clear", "")
	if _, body = a.get(t, draft); strings.Contains(body, `value="Taxi draft"`) {
		t.Error("cleared draft should not show the old description")
	}
	if _, groupBody = a.get(t, "/groups/1"); strings.Contains(groupBody, "unfinished expense") {
		t.Error("cleared draft should not be offered for resuming")
	}

	// Discard (the banner's button) deletes the draft outright.
	if resp := a.put(t, draft, "description=Taxi+draft&date=2026-08-30&split_mode=even&payer_id_0=1&payer_amount_0=10.00"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("autosave: %d, want 204", resp.StatusCode)
	}
	a.postOK(t, draft+"/discard", "")
	if resp := a.get2(t, draft); resp.StatusCode != http.StatusNotFound {
		t.Errorf("discarded draft: %d, want 404", resp.StatusCode)
	}
	if _, groupBody = a.get(t, "/groups/1"); strings.Contains(groupBody, "unfinished expense") {
		t.Error("discarded draft should not bring the banner back")
	}
	if again := a.openDraft(t, 1); again == draft {
		t.Error("discarding should not reopen the same draft")
	} else {
		draft = again
	}

	// Submit turns the draft into an expense and removes it.
	a.postOK(t, draft+"/submit", strings.Join([]string{
		"description=Pizza", "date=2026-08-30", "split_mode=even",
		"payer_id_0=1", "payer_amount_0=20.00", "participant_1=on", "participant_2=on",
	}, "&"))
	if resp := a.get2(t, draft); resp.StatusCode != http.StatusNotFound {
		t.Errorf("submitted draft: %d, want 404", resp.StatusCode)
	}
	_, groupBody = a.get(t, "/groups/1")
	if !strings.Contains(groupBody, ">Pizza<") {
		t.Error("submitted expense should be listed on the group page")
	}
	if strings.Contains(groupBody, "unfinished expense") {
		t.Error("draft banner should be gone after submit")
	}
}

// A draft is private: another signed-in user can neither view, autosave,
// clear, discard nor submit it.
func TestDraftOwnership(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Trip")
	draft := a.openDraft(t, 1)

	mallory := a.createUser(t, "Mallory")
	a.loginAs(t, mallory)
	if resp := a.get2(t, draft); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user GET draft: %d, want 404", resp.StatusCode)
	}
	if resp := a.put(t, draft, "description=Sabotage"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user PUT draft: %d, want 404", resp.StatusCode)
	}
	if resp := a.post(t, draft+"/clear", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user clear draft: %d, want 404", resp.StatusCode)
	}
	if resp := a.post(t, draft+"/discard", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user discard draft: %d, want 404", resp.StatusCode)
	}
	if resp := a.post(t, draft+"/submit", "description=Surprise&date=2026-08-30&split_mode=even&payer_id_0=1&payer_amount_0=1.00&participant_1=on"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user submit draft: %d, want 404", resp.StatusCode)
	}

	// The draft is untouched by all of that.
	a.loginAs(t, 1) // Alice
	code, body := a.get(t, draft)
	if code != 200 || strings.Contains(body, "Sabotage") {
		t.Errorf("draft should be intact for its owner: %d", code)
	}
}

// A scan attached to a draft (the normal new-expense flow) is authorised to
// the draft's owner: visible to them after any number of reloads, invisible
// to every other signed-in user.
func TestDraftReceiptAuthorization(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")
	a.postOK(t, "/groups", "name=Flat")

	draft := a.openDraft(t, 1)
	file := a.uploadReceipt(t, strings.TrimPrefix(draft, "/drafts/"))

	code, img := a.get(t, "/receipts/"+file)
	if code != 200 || !strings.Contains(img, "PNG") {
		t.Fatalf("owner should see the draft receipt: %d %q", code, img)
	}

	mallory := a.createUser(t, "Mallory")
	a.loginAs(t, mallory)
	if resp := a.get2(t, "/receipts/"+file); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other user should not see the draft receipt: %d, want 404", resp.StatusCode)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	a := newApp(t)
	for _, path := range []string{"/static/css/app.css", "/static/js/expense-form.js", "/static/js/main.js", "/static/js/util.js", "/static/js/scan.js"} {
		code, body := a.get(t, path)
		if code != 200 || len(body) == 0 {
			t.Errorf("%s: %d (%d bytes)", path, code, len(body))
		}
	}
}

func TestPWAEndpoints(t *testing.T) {
	a := newApp(t)
	a.setupUser(t, "Alice", "alice@example.com", "password123")

	// fetch returns status, selected headers and the body for a GET.
	fetch := func(path string) (int, http.Header, string) {
		t.Helper()
		resp, err := a.client.Get(a.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(body)
	}

	// The service worker is served from the site root (for a "/" scope),
	// marked no-cache so updates are picked up.
	code, header, body := fetch("/sw.js")
	if code != 200 {
		t.Fatalf("sw.js: %d", code)
	}
	if ct := header.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("sw.js content type: %q", ct)
	}
	if cc := header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("sw.js cache control: %q, want no-cache", cc)
	}
	if !strings.Contains(body, "addEventListener") {
		t.Error("sw.js should contain worker logic")
	}

	// The manifest is valid JSON naming the app and its icons.
	code, header, body = fetch("/manifest.webmanifest")
	if code != 200 {
		t.Fatalf("manifest: %d", code)
	}
	if ct := header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("manifest content type: %q", ct)
	}
	var manifest struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(body), &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest.Name != "Splitpayments" || manifest.Scope != "/" || len(manifest.Icons) == 0 {
		t.Errorf("manifest incomplete: %+v", manifest)
	}

	// Manifest icons and the offline fallback page resolve.
	for _, path := range []string{"/static/icons/icon-192.png", "/static/icons/icon-maskable-512.png", "/static/offline.html", "/static/css/offline.css"} {
		if code, body := a.get(t, path); code != 200 || len(body) == 0 {
			t.Errorf("%s: %d (%d bytes)", path, code, len(body))
		}
	}

	// The layout links the manifest and carries the mobile install metas.
	_, page := a.get(t, "/")
	for _, want := range []string{
		`rel="manifest"`, `name="theme-color"`, `rel="apple-touch-icon"`,
		`name="apple-mobile-web-app-capable"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("layout missing %q", want)
		}
	}
}

func TestCSRFBlocksForeignOrigin(t *testing.T) {
	a := newApp(t)
	req, _ := http.NewRequest(http.MethodPost, a.srv.URL+"/groups", bytes.NewBufferString("name=X"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d, want 403", resp.StatusCode)
	}
}
