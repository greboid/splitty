package expense

// Draft expenses: a saved in-progress "new expense" form. There is one draft
// per user and group — clicking "Add expense" opens it (creating a blank one
// first) and the form autosaves into FormBody, a urlencoded snapshot of the
// same fields the normal form POST carries. Everything else (validation,
// parsing) reuses the ordinary form machinery: a draft is rendered by parsing
// FormBody with ParseExpenseForm and submitted through the same buildExpense
// path as a direct POST.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Draft struct {
	ID          string
	UserID      int64
	GroupID     int64
	FormBody    string
	ReceiptFile string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// OpenDraft returns the user's draft for the group, creating a blank one
// when none exists yet.
func (s *Store) OpenDraft(userID, groupID int64) (Draft, error) {
	d, found, err := s.FindDraft(userID, groupID)
	if err != nil {
		return d, err
	}
	if found {
		return d, nil
	}
	id, err := newDraftID()
	if err != nil {
		return d, err
	}
	// OR IGNORE: two tabs racing the first "Add expense" click; the loser
	// simply re-reads the winner's draft below.
	if _, err := s.DB.Exec(
		`INSERT OR IGNORE INTO expense_drafts (id, user_id, group_id) VALUES (?, ?, ?)`,
		id, userID, groupID); err != nil {
		return d, err
	}
	d, found, err = s.FindDraft(userID, groupID)
	if err != nil {
		return d, err
	}
	if !found {
		return d, errors.New("draft insert ignored but no draft found")
	}
	return d, nil
}

// FindDraft returns the user's draft for the group without creating one.
func (s *Store) FindDraft(userID, groupID int64) (Draft, bool, error) {
	d, err := s.draftQuery(
		`SELECT id, user_id, group_id, form_body, receipt_file, created_at, updated_at
		FROM expense_drafts WHERE user_id = ? AND group_id = ?`, userID, groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, false, nil
	}
	if err != nil {
		return Draft{}, false, err
	}
	return d, true, nil
}

// DraftByID loads one draft; ErrNotFound when the id is unknown.
func (s *Store) DraftByID(id string) (Draft, error) {
	d, err := s.draftQuery(
		`SELECT id, user_id, group_id, form_body, receipt_file, created_at, updated_at
		FROM expense_drafts WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Draft{}, ErrNotFound
	}
	return d, err
}

func (s *Store) draftQuery(query string, args ...any) (Draft, error) {
	var d Draft
	err := s.DB.QueryRow(query, args...).Scan(
		&d.ID, &d.UserID, &d.GroupID, &d.FormBody, &d.ReceiptFile, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// SaveDraftBody stores an autosaved or submitted form snapshot. receiptFile
// is lifted out of the body so receipt serving can authorise on it; the
// caller is responsible for passing only validated file names.
func (s *Store) SaveDraftBody(id, formBody, receiptFile string) error {
	_, err := s.DB.Exec(
		`UPDATE expense_drafts SET form_body = ?, receipt_file = ?, updated_at = ? WHERE id = ?`,
		formBody, receiptFile, time.Now(), id)
	return err
}

// AttachReceipt records a freshly scanned receipt against the draft, closing
// the gap between the upload and the next form autosave. The user_id
// condition keeps the ownership check next to the write.
func (s *Store) AttachReceipt(draftID string, userID int64, file string) error {
	_, err := s.DB.Exec(
		`UPDATE expense_drafts SET receipt_file = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		file, time.Now(), draftID, userID)
	return err
}

// ClearDraft blanks the draft: back to an empty form with no receipt. The
// draft itself stays, so the same URL keeps working.
func (s *Store) ClearDraft(id string) error {
	_, err := s.DB.Exec(
		`UPDATE expense_drafts SET form_body = '', receipt_file = '', updated_at = ? WHERE id = ?`,
		time.Now(), id)
	return err
}

// DeleteDraft removes the draft, e.g. after it became a real expense.
func (s *Store) DeleteDraft(id string) error {
	_, err := s.DB.Exec(`DELETE FROM expense_drafts WHERE id = ?`, id)
	return err
}

// DraftOwnerByReceiptFile finds the owner of the draft a receipt file is
// attached to, for authorising receipt image downloads before the expense
// exists.
func (s *Store) DraftOwnerByReceiptFile(file string) (int64, bool, error) {
	var userID int64
	err := s.DB.QueryRow(
		`SELECT user_id FROM expense_drafts WHERE receipt_file = ?`, file).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return userID, true, nil
}

// HasContent reports whether the draft holds anything worth resuming: a
// blank draft (created by an aborted "Add expense" click) is not offered
// for resuming or discarding.
func (d Draft) HasContent() bool {
	return strings.TrimSpace(d.FormBody) != "" || d.ReceiptFile != ""
}

// newDraftID mints an unguessable draft id, same shape as receipt file
// names (duplicated from the receipt package, which imports this package).
func newDraftID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}
