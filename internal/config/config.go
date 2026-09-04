// Package config defines every command-line flag (each also settable via an
// UPPER_SNAKE_CASE environment variable thanks to envflag) and the resolved
// Config struct used across the app.
package config

import (
	"flag"
	"time"
)

type Config struct {
	Listen         string
	DBDriver       string
	Database       string
	StaticDir      string
	Currency       string
	CurrencySymbol string

	ReceiptAPIURL      string
	ReceiptAPIKey      string
	ReceiptModel       string
	ReceiptAPIFormat   string
	ReceiptAPIWorkflow string
	ReceiptAPIArgs     string

	SessionLifetime time.Duration
	InviteLifetime  time.Duration

	TrustedProxies []string
	CookieSecure   bool
}

// Register defines all flags on the given flag set. Called with
// flag.CommandLine before envflag.Parse.
func Register(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.Listen, "listen", ":8080", "HTTP listen address")
	fs.StringVar(&c.DBDriver, "db-driver", "sqlite", "database backend: sqlite (also sqlite3) or postgres (also pg/postgresql)")
	fs.StringVar(&c.Database, "database", "data/splitpayments.db", "for SQLite, the database file path (receipt images are stored in it too); for Postgres, a libpq-style connection string")
	fs.StringVar(&c.StaticDir, "static-dir", "", "Serve static assets from this directory instead of the embedded copies (dev only)")
	fs.StringVar(&c.Currency, "currency", "GBP", "Install-wide ISO 4217 currency code")
	fs.StringVar(&c.CurrencySymbol, "currency-symbol", "£", "Currency symbol used in templates and the client-side formatter")

	fs.StringVar(&c.ReceiptAPIURL, "receipt-api-url", "", "API base URL for receipt scanning (OpenAI-compatible, Anthropic, gwtype1, gwtype2 or gwtype3; empty disables the feature)")
	fs.StringVar(&c.ReceiptAPIKey, "receipt-api-key", "", "API key sent to the receipt API (Bearer token, or x-api-key for Anthropic)")
	fs.StringVar(&c.ReceiptModel, "receipt-model", "", "Vision model name used for receipt scanning")
	fs.StringVar(&c.ReceiptAPIFormat, "receipt-api-format", "", "Receipt API protocol: \"openai\" (default), \"anthropic\", \"gwtype1\", \"gwtype2\" or \"gwtype3\" (workflow gateways); empty auto-detects Anthropic from the URL")
	fs.StringVar(&c.ReceiptAPIWorkflow, "receipt-api-workflow", "", "Workflow executed on the gwtype2 or gwtype3 gateway")
	fs.StringVar(&c.ReceiptAPIArgs, "receipt-api-args", "", "JSON object sent as the workflow args to the gwtype2/gwtype3 gateway (default \"{}\")")

	fs.DurationVar(&c.SessionLifetime, "session-lifetime", 720*time.Hour, "How long login sessions last")
	fs.DurationVar(&c.InviteLifetime, "invite-lifetime", 72*time.Hour, "How long invite links remain valid")

	fs.Var(&repeatableStrings{v: &c.TrustedProxies}, "trusted-proxy", "Additional trusted proxy CIDR for X-Forwarded-For; repeatable (private ranges always trusted)")
	fs.BoolVar(&c.CookieSecure, "cookie-secure", true, "Mark the session cookie Secure (disable for plain-HTTP local dev)")
	return c
}

// repeatableStrings implements flag.Value for flags that may be given
// multiple times, accumulating into a string slice.
type repeatableStrings struct {
	v *[]string
}

func (r *repeatableStrings) String() string {
	if r == nil || r.v == nil {
		return ""
	}
	out := ""
	for i, s := range *r.v {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func (r *repeatableStrings) Set(s string) error {
	*r.v = append(*r.v, s)
	return nil
}
