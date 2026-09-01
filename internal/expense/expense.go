// Package expense holds the Expense type, the split-mode computation, the
// store and the expense pages.
//
// The core model: an expense is just shares. Each participant has a `paid`
// amount (what they put in) and an `owed` amount (their share of the cost).
// Σ paid == Σ owed == the expense total. Multiple payers, every split mode
// and settle-up payments are all this one shape; a payment is simply a row
// where the payer's `paid` equals the recipient's `owed`.
package expense

import "time"

// Split modes supported by the form and ComputeOwed.
const (
	SplitEven     = "even"
	SplitExact    = "exact"
	SplitPercent  = "percent"
	SplitShares   = "shares"
	SplitItemized = "itemized"
)

// Categories offered in the form; stored as plain strings.
var Categories = []string{
	"general", "groceries", "dining", "utilities", "rent",
	"travel", "entertainment", "shopping", "health", "other",
}

type Share struct {
	UserID int64
	Paid   int64
	Owed   int64
}

type Item struct {
	ID          int64
	Position    int
	Description string
	Amount      int64
	Assignees   []int64 // split evenly among these
}

type Expense struct {
	ID          int64
	GroupID     int64
	Description string
	Notes       string
	Category    string
	Date        string // YYYY-MM-DD
	IsPayment   bool
	SplitMode   string
	ReceiptFile string
	CreatedBy   int64
	CreatedAt   time.Time
	Shares      []Share
	Items       []Item
}

// The Expense methods use value receivers so templates can call them on
// non-addressable fields (e.g. {{money .Expense.Total}} on a page struct
// passed by value; a pointer receiver silently aborts the render there).

// Total is the sum of paid amounts (== sum of owed for valid expenses).
func (e Expense) Total() int64 {
	var t int64
	for _, s := range e.Shares {
		t += s.Paid
	}
	return t
}

// Payers returns the participants who put money in, in share order.
func (e Expense) Payers() []Share {
	var out []Share
	for _, s := range e.Shares {
		if s.Paid > 0 {
			out = append(out, s)
		}
	}
	return out
}

// OwedBy returns the owed amount for a user (0 when absent).
func (e Expense) OwedBy(userID int64) int64 {
	for _, s := range e.Shares {
		if s.UserID == userID {
			return s.Owed
		}
	}
	return 0
}
