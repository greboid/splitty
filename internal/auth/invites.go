package auth

// Invite links let someone without an account (or an existing guest) claim
// one, optionally joining a group at the same time. Only the SHA-256 of the
// token is stored.

import (
	"database/sql"
	"errors"
	"time"
)

type Invite struct {
	ID          int64
	Email       string
	GroupID     sql.NullInt64
	GroupName   string
	InviterName string
	CreatedBy   int64
	ExpiresAt   time.Time
	AcceptedBy  sql.NullInt64
	AcceptedAt  sql.NullTime
}

func (i Invite) Expired() bool  { return time.Now().After(i.ExpiresAt) }
func (i Invite) Accepted() bool { return i.AcceptedBy.Valid }

var ErrNotFound = errors.New("invite not found")

type InviteStore struct {
	DB       *sql.DB
	Lifetime time.Duration
}

// Create inserts an invite and returns the unhashed token to hand to the
// invitee.
func (s *InviteStore) Create(email string, groupID, createdBy int64) (string, error) {
	token, hash, err := NewToken()
	if err != nil {
		return "", err
	}
	var gid any
	if groupID != 0 {
		gid = groupID
	}
	_, err = s.DB.Exec(
		`INSERT INTO invites (email, group_id, token_hash, created_by, expires_at) VALUES (?, ?, ?, ?, ?)`,
		email, gid, hash, createdBy, time.Now().Add(s.Lifetime))
	if err != nil {
		return "", err
	}
	return token, nil
}

func (s *InviteStore) GetByToken(token string) (Invite, error) {
	var i Invite
	err := s.DB.QueryRow(`
		SELECT i.id, i.email, i.group_id, COALESCE(g.name, ''), COALESCE(u.name, ''),
		       i.created_by, i.expires_at, i.accepted_by, i.accepted_at
		FROM invites i
		LEFT JOIN groups g ON g.id = i.group_id
		LEFT JOIN users u ON u.id = i.created_by
		WHERE i.token_hash = ?`, HashToken(token)).Scan(
		&i.ID, &i.Email, &i.GroupID, &i.GroupName, &i.InviterName,
		&i.CreatedBy, &i.ExpiresAt, &i.AcceptedBy, &i.AcceptedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

func (s *InviteStore) Accept(id, acceptedBy int64) error {
	_, err := s.DB.Exec(`UPDATE invites SET accepted_by = ?, accepted_at = ? WHERE id = ?`,
		acceptedBy, time.Now(), id)
	return err
}

// PendingForGroup lists unaccepted invites for the group page.
func (s *InviteStore) PendingForGroup(groupID int64) ([]Invite, error) {
	rows, err := s.DB.Query(`
		SELECT i.id, i.email, i.group_id, COALESCE(g.name, ''), COALESCE(u.name, ''),
		       i.created_by, i.expires_at, i.accepted_by, i.accepted_at
		FROM invites i
		LEFT JOIN groups g ON g.id = i.group_id
		LEFT JOIN users u ON u.id = i.created_by
		WHERE i.group_id = ? AND i.accepted_by IS NULL
		ORDER BY i.id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.Email, &i.GroupID, &i.GroupName, &i.InviterName,
			&i.CreatedBy, &i.ExpiresAt, &i.AcceptedBy, &i.AcceptedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
