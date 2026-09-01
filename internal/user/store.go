package user

import (
	"database/sql"
	"errors"
)

type Store struct{ DB *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{DB: db} }

const userColumns = `id, email, name, password_hash, is_guest, invited_by, created_at`

func scanUser(s rowScanner) (User, error) {
	var u User
	var email, hash sql.NullString
	err := s.Scan(&u.ID, &email, &u.Name, &hash, &u.IsGuest, &u.InvitedBy, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, err
	}
	u.Email = email.String
	u.PasswordHash = hash.String
	return u, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) ByID(id int64) (User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// GuestByName finds a name-only user by exact name (case-insensitive).
func (s *Store) GuestByName(name string) (User, bool, error) {
	u, err := scanUser(s.DB.QueryRow(
		`SELECT `+userColumns+` FROM users WHERE is_guest = TRUE AND lower(name) = lower(?) LIMIT 1`, name))
	if errors.Is(err, ErrNotFound) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (s *Store) ByEmail(email string) (User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userColumns+` FROM users WHERE lower(email) = lower(?)`, email))
}

// Count reports how many accounts exist; zero means the instance still needs
// first-run setup.
func (s *Store) Count() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) Create(u *User) error {
	res, err := s.DB.Exec(
		`INSERT INTO users (email, name, password_hash, is_guest, invited_by) VALUES (?, ?, ?, ?, ?)`,
		nullString(u.Email), u.Name, nullString(u.PasswordHash), u.IsGuest, u.InvitedBy,
	)
	if err != nil {
		return err
	}
	u.ID, err = res.LastInsertId()
	return err
}

// UpdateCredentials upgrades a guest to a full account (or renames an
// existing one), setting name, email and password in one go.
func (s *Store) UpdateCredentials(id int64, name, email, passwordHash string) error {
	_, err := s.DB.Exec(`UPDATE users SET name = ?, email = ?, password_hash = ?, is_guest = FALSE WHERE id = ?`,
		name, nullString(email), nullString(passwordHash), id)
	return err
}

func (s *Store) UpdateName(id int64, name string) error {
	_, err := s.DB.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, id)
	return err
}

// ByIDs resolves multiple users in one query per id; callers keep their own
// ordering. Instance scale is small, so N queries are fine.
func (s *Store) ByIDs(ids []int64) ([]User, error) {
	out := make([]User, 0, len(ids))
	for _, id := range ids {
		u, err := s.ByID(id)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
