package group

import (
	"database/sql"
	"errors"
	"strconv"

	"github.com/greboid/splitpayments/internal/user"
)

var ErrNotFound = errors.New("group not found")

type Store struct{ DB *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

const groupColumns = `id, name, kind, created_by, created_at`

func scanGroup(s interface{ Scan(...any) error }) (Group, error) {
	var g Group
	err := s.Scan(&g.ID, &g.Name, &g.Kind, &g.CreatedBy, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

func (s *Store) ByID(id int64) (Group, error) {
	return scanGroup(s.DB.QueryRow(`SELECT `+groupColumns+` FROM groups WHERE id = ?`, id))
}

// GroupName is the narrow lookup the auth invite flow uses.
func (s *Store) GroupName(id int64) (string, error) {
	var name string
	err := s.DB.QueryRow(`SELECT name FROM groups WHERE id = ?`, id).Scan(&name)
	return name, err
}

func (s *Store) Create(name, kind string, createdBy int64) (Group, error) {
	var id int64
	// RETURNING works on both SQLite and Postgres.
	if err := s.DB.QueryRow(`INSERT INTO groups (name, kind, created_by) VALUES (?, ?, ?) RETURNING id`,
		name, kind, createdBy).Scan(&id); err != nil {
		return Group{}, err
	}
	return s.ByID(id)
}

// ForUser lists the visible groups (kind='group') the user belongs to.
func (s *Store) ForUser(userID int64) ([]Group, error) {
	rows, err := s.DB.Query(`
		SELECT `+groupColumns+` FROM groups g
		JOIN memberships m ON m.group_id = g.id
		WHERE m.user_id = ? AND g.kind = 'group'
		ORDER BY lower(g.name)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Friends lists the users with whom the user has a direct ledger.
func (s *Store) Friends(userID int64) ([]user.User, error) {
	rows, err := s.DB.Query(`
		SELECT u.id, u.email, u.name, u.password_hash, u.is_guest, u.invited_by, u.created_at
		FROM users u
		JOIN memberships m1 ON m1.user_id = u.id
		JOIN memberships m2 ON m2.group_id = m1.group_id
		JOIN groups g ON g.id = m1.group_id
		WHERE m2.user_id = ? AND u.id != ? AND g.kind = 'direct'
		GROUP BY u.id
		ORDER BY lower(u.name)`, userID, userID)
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

// MemberIDs returns the member user ids of a group.
func (s *Store) MemberIDs(groupID int64) ([]int64, error) {
	rows, err := s.DB.Query(`SELECT user_id FROM memberships WHERE group_id = ? ORDER BY user_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Members returns the member users of a group ordered by name.
func (s *Store) Members(groupID int64) ([]user.User, error) {
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

func (s *Store) IsMember(groupID, userID int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM memberships WHERE group_id = ? AND user_id = ?`, groupID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) AddMember(groupID, userID int64) error {
	// ON CONFLICT (unlike INSERT OR IGNORE) runs on both SQLite and Postgres.
	_, err := s.DB.Exec(`INSERT INTO memberships (group_id, user_id) VALUES (?, ?)
		ON CONFLICT (group_id, user_id) DO NOTHING`, groupID, userID)
	return err
}

func (s *Store) RemoveMember(groupID, userID int64) error {
	_, err := s.DB.Exec(`DELETE FROM memberships WHERE group_id = ? AND user_id = ?`, groupID, userID)
	return err
}

func (s *Store) Update(groupID int64, name string) error {
	_, err := s.DB.Exec(`UPDATE groups SET name = ? WHERE id = ?`, name, groupID)
	return err
}

// DirectBetween finds the hidden two-person ledger between a and b, creating
// it (with both memberships) when it does not exist yet.
func (s *Store) DirectBetween(a, b int64) (Group, error) {
	var id int64
	err := s.DB.QueryRow(`
		SELECT g.id FROM groups g
		WHERE g.kind = 'direct'
		  AND (SELECT COUNT(*) FROM memberships m WHERE m.group_id = g.id) = 2
		  AND EXISTS (SELECT 1 FROM memberships m WHERE m.group_id = g.id AND m.user_id = ?)
		  AND EXISTS (SELECT 1 FROM memberships m WHERE m.group_id = g.id AND m.user_id = ?)
		LIMIT 1`, a, b).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		g, err := s.Create(directName(a, b), KindDirect, a)
		if err != nil {
			return Group{}, err
		}
		if err := s.AddMember(g.ID, a); err != nil {
			return Group{}, err
		}
		if err := s.AddMember(g.ID, b); err != nil {
			return Group{}, err
		}
		return g, nil
	}
	if err != nil {
		return Group{}, err
	}
	return s.ByID(id)
}

// directName is a label only ever seen if the ledger leaks into a list; the
// real name is rendered from the other member.
func directName(a, b int64) string {
	if a > b {
		a, b = b, a
	}
	return "direct:" + strconv.FormatInt(a, 10) + ":" + strconv.FormatInt(b, 10)
}
