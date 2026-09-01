// Package user holds the User type and its CRUD store.
package user

import (
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned when no matching user exists.
var ErrNotFound = errors.New("user not found")

// User is a participant. A regular account has an email and password and can
// log in. A guest (IsGuest) has only a name: it is created by group members
// to track debts for someone without an account, can never log in, and can
// later be upgraded to a full account by accepting an invite for their email.
type User struct {
	ID           int64
	Email        string // empty for guests
	Name         string
	PasswordHash string // empty for guests
	IsGuest      bool
	InvitedBy    sql.NullInt64
	CreatedAt    time.Time
}
