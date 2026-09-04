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

func TestResolveDriver(t *testing.T) {
	cases := []struct {
		name, driver, dsn, want string
		wantErr                 bool
	}{
		{"empty dsn defaults to sqlite", "", "data/splitpayments.db", "sqlite", false},
		{"url detected", "", "postgres://splitty:splitty@db:5432/splitty", "postgres", false},
		{"postgresql url detected", "", "postgresql://db/splitty", "postgres", false},
		{"key/value detected", "", "host=db user=splitty password=splitty dbname=splitty", "postgres", false},
		{"explicit driver wins over url", "sqlite", "postgres://db/splitty", "sqlite", false},
		{"alias", "pg", "anything", "postgres", false},
		{"unknown driver", "mysql", "postgres://db/splitty", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveDriver(tc.driver, tc.dsn)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ResolveDriver(%q, %q) err = %v, wantErr %v", tc.name, tc.driver, tc.dsn, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: ResolveDriver(%q, %q) = %q, want %q", tc.name, tc.driver, tc.dsn, got, tc.want)
		}
	}
}

func TestSSLModeSpecified(t *testing.T) {
	cases := []struct {
		dsn  string
		want bool
	}{
		{"postgres://u:p@db:5432/splitty", false},
		{"postgres://u:p@db:5432/splitty?sslmode=disable", true},
		{"postgres://u:p@db:5432/splitty?sslmode=require&application_name=x", true},
		{"postgres://u:p@db:5432/splitty?sslrootcert=/ca.pem", true},
		{"postgres://u:p@db:5432/splitty?application_name=x", false},
		{"host=db sslmode=require", true},
		{"host=db dbname=splitty", false},
	}
	for _, tc := range cases {
		if got := sslModeSpecified(tc.dsn); got != tc.want {
			t.Errorf("sslModeSpecified(%q) = %v, want %v", tc.dsn, got, tc.want)
		}
	}
}
