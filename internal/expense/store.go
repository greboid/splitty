package expense

import (
	"database/sql"
	"errors"
	"time"

	"github.com/greboid/splitpayments/internal/user"
)

var ErrNotFound = errors.New("expense not found")

type Store struct{ DB *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// Insert persists the expense with its shares and items in one transaction.
func (s *Store) Insert(e *Expense) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// RETURNING works on both SQLite and Postgres (LastInsertId does not).
	if err := tx.QueryRow(`
		INSERT INTO expenses (group_id, description, notes, category, date, is_payment, split_mode, receipt_file, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		e.GroupID, e.Description, e.Notes, e.Category, e.Date, e.IsPayment,
		e.SplitMode, nullString(e.ReceiptFile), e.CreatedBy).Scan(&e.ID); err != nil {
		return err
	}
	if err := insertShares(tx, e); err != nil {
		return err
	}
	if err := insertItems(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

// Update replaces the mutable fields, shares and items of an existing
// expense.
func (s *Store) Update(e *Expense) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		UPDATE expenses SET description = ?, notes = ?, category = ?, date = ?, split_mode = ?, receipt_file = ?, updated_at = ?
		WHERE id = ?`,
		e.Description, e.Notes, e.Category, e.Date, e.SplitMode, nullString(e.ReceiptFile), time.Now(), e.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM expense_shares WHERE expense_id = ?`, e.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM expense_items WHERE expense_id = ?`, e.ID); err != nil {
		return err
	}
	if err := insertShares(tx, e); err != nil {
		return err
	}
	if err := insertItems(tx, e); err != nil {
		return err
	}
	return tx.Commit()
}

func insertShares(tx *sql.Tx, e *Expense) error {
	for _, sh := range e.Shares {
		// ON CONFLICT (unlike INSERT OR REPLACE) runs on both SQLite and
		// Postgres; the composite primary key guards the upsert.
		if _, err := tx.Exec(
			`INSERT INTO expense_shares (expense_id, user_id, paid, owed) VALUES (?, ?, ?, ?)
			ON CONFLICT (expense_id, user_id) DO UPDATE SET paid = EXCLUDED.paid, owed = EXCLUDED.owed`,
			e.ID, sh.UserID, sh.Paid, sh.Owed); err != nil {
			return err
		}
	}
	return nil
}

func insertItems(tx *sql.Tx, e *Expense) error {
	for pos, it := range e.Items {
		var itemID int64
		if err := tx.QueryRow(
			`INSERT INTO expense_items (expense_id, position, description, amount) VALUES (?, ?, ?, ?) RETURNING id`,
			e.ID, pos, it.Description, it.Amount).Scan(&itemID); err != nil {
			return err
		}
		for _, u := range it.Assignees {
			if _, err := tx.Exec(
				`INSERT INTO item_shares (item_id, user_id) VALUES (?, ?)
				ON CONFLICT (item_id, user_id) DO NOTHING`, itemID, u); err != nil {
				return err
			}
		}
	}
	return nil
}

// SoftDelete marks the expense deleted; it disappears from lists and
// balances but stays in the database.
func (s *Store) SoftDelete(id int64) error {
	_, err := s.DB.Exec(`UPDATE expenses SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, time.Now(), id)
	return err
}

// MemberList lists a group's members. This duplicates a query the group
// package also performs, but keeps expense free of a group import (which
// would cycle, since group pages list expenses).
func (s *Store) MemberList(groupID int64) ([]user.User, error) {
	rows, err := s.DB.Query(`
		SELECT u.id, u.email, u.name, u.password_hash, u.is_guest, u.invited_by, u.created_at
		FROM users u JOIN memberships m ON m.user_id = u.id
		WHERE m.group_id = ?
		ORDER BY lower(u.name)`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []user.User
	for rows.Next() {
		var u user.User
		var email, hash sql.NullString
		if err := rows.Scan(&u.ID, &email, &u.Name, &hash, &u.IsGuest, &u.InvitedBy, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Email = email.String
		u.PasswordHash = hash.String
		out = append(out, u)
	}
	return out, rows.Err()
}

// GroupName fetches just the group's name for headings.
func (s *Store) GroupName(groupID int64) (string, error) {
	var name string
	err := s.DB.QueryRow(`SELECT name FROM groups WHERE id = ?`, groupID).Scan(&name)
	return name, err
}

// GroupIDByReceiptFile finds the group of the expense a receipt file is
// attached to, for authorising receipt image downloads.
func (s *Store) GroupIDByReceiptFile(file string) (int64, bool, error) {
	var groupID int64
	err := s.DB.QueryRow(
		`SELECT group_id FROM expenses WHERE receipt_file = ? AND deleted_at IS NULL`, file).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return groupID, true, nil
}

// IsMember reports whether the user can see the given expense's group.
func (s *Store) IsMember(groupID, userID int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM memberships WHERE group_id = ? AND user_id = ?`, groupID, userID).Scan(&n)
	return n > 0, err
}

// ByID loads one expense with shares and items attached.
func (s *Store) ByID(id int64) (Expense, error) {
	var e Expense
	var receipt sql.NullString
	err := s.DB.QueryRow(`
		SELECT id, group_id, description, notes, category, date, is_payment, split_mode, receipt_file, created_by, created_at
		FROM expenses WHERE id = ? AND deleted_at IS NULL`, id).Scan(
		&e.ID, &e.GroupID, &e.Description, &e.Notes, &e.Category, &e.Date, &e.IsPayment,
		&e.SplitMode, &receipt, &e.CreatedBy, &e.CreatedAt)
	e.Date = normalizeDate(e.Date)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	e.ReceiptFile = receipt.String
	if err := s.loadShares(&e); err != nil {
		return e, err
	}
	if err := s.loadItems(&e); err != nil {
		return e, err
	}
	return e, nil
}

// ListForGroup loads all non-deleted expenses of a group (payments
// included), newest date first, with shares and items attached.
func (s *Store) ListForGroup(groupID int64) ([]Expense, error) {
	rows, err := s.DB.Query(`
		SELECT id, group_id, description, notes, category, date, is_payment, split_mode, receipt_file, created_by, created_at
		FROM expenses WHERE group_id = ? AND deleted_at IS NULL
		ORDER BY date DESC, id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Expense
	for rows.Next() {
		var e Expense
		var receipt sql.NullString
		if err := rows.Scan(&e.ID, &e.GroupID, &e.Description, &e.Notes, &e.Category, &e.Date,
			&e.IsPayment, &e.SplitMode, &receipt, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Date = normalizeDate(e.Date)
		e.ReceiptFile = receipt.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := s.attachShares(out); err != nil {
		return nil, err
	}
	if err := s.attachItems(out); err != nil {
		return nil, err
	}
	return out, nil
}

// BalancesRows yields flat share rows (paid/owed per user per expense) for
// the balance engine, in one query.
type BalanceRow struct {
	UserID int64
	Paid   int64
	Owed   int64
}

func (s *Store) BalanceRows(groupID int64) ([]BalanceRow, error) {
	rows, err := s.DB.Query(`
		SELECT es.user_id, es.paid, es.owed
		FROM expense_shares es JOIN expenses e ON e.id = es.expense_id
		WHERE e.group_id = ? AND e.deleted_at IS NULL`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BalanceRow
	for rows.Next() {
		var r BalanceRow
		if err := rows.Scan(&r.UserID, &r.Paid, &r.Owed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) loadShares(e *Expense) error {
	rows, err := s.DB.Query(
		`SELECT user_id, paid, owed FROM expense_shares WHERE expense_id = ? ORDER BY paid DESC, user_id`, e.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sh Share
		if err := rows.Scan(&sh.UserID, &sh.Paid, &sh.Owed); err != nil {
			return err
		}
		e.Shares = append(e.Shares, sh)
	}
	return rows.Err()
}

func (s *Store) loadItems(e *Expense) error {
	rows, err := s.DB.Query(
		`SELECT i.id, i.position, i.description, i.amount FROM expense_items i WHERE i.expense_id = ? ORDER BY i.position`, e.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Position, &it.Description, &it.Amount); err != nil {
			return err
		}
		e.Items = append(e.Items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Assignees per item in a second pass to keep the main scan simple.
	for i := range e.Items {
		ids, err := s.DB.Query(`SELECT user_id FROM item_shares WHERE item_id = ? ORDER BY user_id`, e.Items[i].ID)
		if err != nil {
			return err
		}
		for ids.Next() {
			var u int64
			if err := ids.Scan(&u); err != nil {
				ids.Close()
				return err
			}
			e.Items[i].Assignees = append(e.Items[i].Assignees, u)
		}
		ids.Close()
	}
	return nil
}

func (s *Store) attachShares(expenses []Expense) error {
	rows, err := s.DB.Query(`
		SELECT es.expense_id, es.user_id, es.paid, es.owed
		FROM expense_shares es JOIN expenses e ON e.id = es.expense_id
		WHERE e.group_id = ? AND e.deleted_at IS NULL`, expenses[0].GroupID)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[int64]*Expense{}
	for i := range expenses {
		byID[expenses[i].ID] = &expenses[i]
	}
	for rows.Next() {
		var eid, uid, paid, owed int64
		if err := rows.Scan(&eid, &uid, &paid, &owed); err != nil {
			return err
		}
		if e := byID[eid]; e != nil {
			e.Shares = append(e.Shares, Share{UserID: uid, Paid: paid, Owed: owed})
		}
	}
	return rows.Err()
}

func (s *Store) attachItems(expenses []Expense) error {
	rows, err := s.DB.Query(`
		SELECT i.expense_id, i.id, i.position, i.description, i.amount
		FROM expense_items i JOIN expenses e ON e.id = i.expense_id
		WHERE e.group_id = ? AND e.deleted_at IS NULL
		ORDER BY i.position`, expenses[0].GroupID)
	if err != nil {
		return err
	}
	defer rows.Close()
	byExp := map[int64]*Expense{}
	for i := range expenses {
		byExp[expenses[i].ID] = &expenses[i]
	}
	for rows.Next() {
		var eid, id, pos, amount int64
		var desc string
		if err := rows.Scan(&eid, &id, &pos, &desc, &amount); err != nil {
			return err
		}
		if e := byExp[eid]; e != nil {
			e.Items = append(e.Items, Item{ID: id, Position: int(pos), Description: desc, Amount: amount})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	byItem := map[int64]*Item{}
	for i := range expenses {
		for j := range expenses[i].Items {
			byItem[expenses[i].Items[j].ID] = &expenses[i].Items[j]
		}
	}
	assignees, err := s.DB.Query(`
		SELECT s.item_id, s.user_id FROM item_shares s
		JOIN expense_items i ON i.id = s.item_id
		JOIN expenses e ON e.id = i.expense_id
		WHERE e.group_id = ? AND e.deleted_at IS NULL
		ORDER BY s.user_id`, expenses[0].GroupID)
	if err != nil {
		return err
	}
	defer assignees.Close()
	for assignees.Next() {
		var itemID, uid int64
		if err := assignees.Scan(&itemID, &uid); err != nil {
			return err
		}
		if it, ok := byItem[itemID]; ok {
			it.Assignees = append(it.Assignees, uid)
		}
	}
	return assignees.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// normalizeDate trims the time part SQLite adds when a DATE column comes
// back through the driver, leaving YYYY-MM-DD.
func normalizeDate(s string) string {
	if len(s) > 10 && (s[10] == 'T' || s[10] == ' ') {
		return s[:10]
	}
	return s
}
