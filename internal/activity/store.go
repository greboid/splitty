// Package activity appends and queries the global/per-group activity feed.
package activity

import (
	"database/sql"
	"time"
)

// Types of feed entries.
const (
	GroupCreated   = "group_created"
	MemberAdded    = "member_added"
	ExpenseAdded   = "expense_added"
	ExpenseUpdated = "expense_updated"
	ExpenseDeleted = "expense_deleted"
	PaymentAdded   = "payment_added"
	JoinedGroup    = "joined_group"
)

type Entry struct {
	ID        int64
	GroupID   int64 // 0 = global
	ExpenseID int64 // 0 = none
	ActorName string
	ActorID   int64
	Type      string
	Detail    string
	GroupName string
	CreatedAt time.Time
}

type Store struct{ DB *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

// Append records a feed entry. groupID/expenseID/actorID of 0 mean "unset".
func (s *Store) Append(groupID, expenseID, actorID int64, typ, detail string) error {
	_, err := s.DB.Exec(`INSERT INTO activity (group_id, expense_id, actor_id, type, detail) VALUES (?, ?, ?, ?, ?)`,
		nullID(groupID), nullID(expenseID), nullID(actorID), typ, detail)
	return err
}

// ForGroup returns the newest entries for one group.
func (s *Store) ForGroup(groupID int64, limit int) ([]Entry, error) {
	return s.query(`WHERE a.group_id = ?`, groupID, limit)
}

// ForUser returns the newest entries across every group the user belongs to.
func (s *Store) ForUser(userID int64, limit int) ([]Entry, error) {
	return s.query(`WHERE a.group_id IS NULL OR a.group_id IN (SELECT group_id FROM memberships WHERE user_id = ?)`, userID, limit)
}

func (s *Store) query(where string, arg int64, limit int) ([]Entry, error) {
	rows, err := s.DB.Query(`
		SELECT a.id, COALESCE(a.group_id, 0), COALESCE(a.expense_id, 0),
		       COALESCE(u.name, ''), COALESCE(a.actor_id, 0), a.type, a.detail,
		       COALESCE(g.name, ''), a.created_at
		FROM activity a
		LEFT JOIN users u ON u.id = a.actor_id
		LEFT JOIN groups g ON g.id = a.group_id
		`+where+`
		ORDER BY a.id DESC LIMIT ?`, arg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.GroupID, &e.ExpenseID, &e.ActorName, &e.ActorID, &e.Type, &e.Detail, &e.GroupName, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
