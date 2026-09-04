package database

import "testing"

func TestRebind(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"no placeholders", "SELECT 1 FROM users", "SELECT 1 FROM users"},
		{"ordered", "SELECT * FROM users WHERE id = ? AND name = ?", "SELECT * FROM users WHERE id = $1 AND name = $2"},
		{"literal kept", "SELECT 'a?b' WHERE id = ?", "SELECT 'a?b' WHERE id = $1"},
		{"escaped quote in literal", `SELECT 'it''s? fine' WHERE id = ?`, `SELECT 'it''s? fine' WHERE id = $1`},
		{"quoted identifier kept", `SELECT "weird?col" FROM t WHERE id = ?`, `SELECT "weird?col" FROM t WHERE id = $1`},
		{"line comment kept", "SELECT 1 -- what?\nWHERE id = ?", "SELECT 1 -- what?\nWHERE id = $1"},
		{"block comment kept", "SELECT 1 /* what? */ WHERE id = ?", "SELECT 1 /* what? */ WHERE id = $1"},
		{"unterminated block comment", "SELECT 1 /* what?", "SELECT 1 /* what?"},
		{"unterminated literal", "SELECT 'oops? WHERE id = ?", "SELECT 'oops? WHERE id = ?"},
		{"existing dollars untouched", "INSERT INTO t (a) VALUES ($1)", "INSERT INTO t (a) VALUES ($1)"},
	}
	for _, tc := range cases {
		if got := rebind(tc.in); got != tc.want {
			t.Errorf("%s: rebind(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
