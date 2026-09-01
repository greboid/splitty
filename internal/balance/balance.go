// Package balance computes net positions, original pairwise debts and
// simplified (debt-cancelling) transfers from expense shares. All functions
// are pure and operate on int64 minor units.
package balance

import (
	"sort"

	"github.com/greboid/splitpayments/internal/money"
)

// Share is one participant's stake in one expense: what they paid and what
// they owed. Rows for a single expense must be grouped (see Expenses).
type Share struct {
	ExpenseID int64
	UserID    int64
	Paid      int64
	Owed      int64
}

// Expense groups the shares of one non-deleted expense, in any order.
type Expense struct {
	ID        int64
	IsPayment bool
	Shares    []Share
}

// Debt is an amount one user owes another.
type Debt struct {
	From   int64
	To     int64
	Amount int64
}

// Net returns each user's net position: sum of (paid - owed) across all
// expenses. Positive means the user is owed money overall.
func Net(expenses []Expense) map[int64]int64 {
	net := map[int64]int64{}
	for _, e := range expenses {
		for _, s := range e.Shares {
			net[s.UserID] += s.Paid - s.Owed
		}
	}
	return net
}

// Pairwise returns the original debts: for every expense, each ower's owed
// amount is attributed to the payers in proportion to what they paid, and
// accumulated per ordered pair. Payments (one payer, one ower) fall out of
// the same formula. Mutual debts between a pair cancel to a single
// direction; zero debts are omitted.
func Pairwise(expenses []Expense) []Debt {
	type pair struct{ from, to int64 }
	acc := map[pair]int64{}
	for _, e := range expenses {
		var payers []Share
		var paidTotal int64
		for _, s := range e.Shares {
			if s.Paid > 0 {
				payers = append(payers, s)
				paidTotal += s.Paid
			}
		}
		if paidTotal == 0 {
			continue
		}
		weights := make([]int64, len(payers))
		for i, p := range payers {
			weights[i] = p.Paid
		}
		for _, s := range e.Shares {
			if s.Owed == 0 {
				continue
			}
			for i, part := range money.AllocateByWeights(s.Owed, weights) {
				if payers[i].UserID != s.UserID && part != 0 {
					acc[pair{s.UserID, payers[i].UserID}] += part
				}
			}
		}
	}
	out := make([]Debt, 0, len(acc))
	seen := map[pair]bool{}
	for p, v := range acc {
		if seen[p] {
			continue
		}
		seen[p] = true
		opp := pair{p.to, p.from}
		seen[opp] = true
		if d := v - acc[opp]; d > 0 {
			out = append(out, Debt{From: p.from, To: p.to, Amount: d})
		} else if d < 0 {
			out = append(out, Debt{From: p.to, To: p.from, Amount: -d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// Simplify reduces everyone's position to the minimum number of transfers
// using greedy min-cash-flow: the largest debtor pays the largest creditor
// until all nets are zero. The result never changes anyone's net total.
func Simplify(expenses []Expense) []Debt {
	return SimplifyNets(Net(expenses))
}

// SimplifyNets is Simplify over precomputed net positions. Zero positions
// are dropped; output is deterministic (sorted by user id on input, then
// greedily matched).
func SimplifyNets(net map[int64]int64) []Debt {
	type pos struct {
		user int64
		amt  int64
	}
	var creditors, debtors []pos
	for u, v := range net {
		switch {
		case v > 0:
			creditors = append(creditors, pos{u, v})
		case v < 0:
			debtors = append(debtors, pos{u, -v})
		}
	}
	// Deterministic order regardless of map iteration.
	sort.Slice(creditors, func(i, j int) bool { return creditors[i].user < creditors[j].user })
	sort.Slice(debtors, func(i, j int) bool { return debtors[i].user < debtors[j].user })

	var out []Debt
	ci, di := 0, 0
	for ci < len(creditors) && di < len(debtors) {
		c, d := &creditors[ci], &debtors[di]
		amount := min(c.amt, d.amt)
		out = append(out, Debt{From: d.user, To: c.user, Amount: amount})
		c.amt -= amount
		d.amt -= amount
		if c.amt == 0 {
			ci++
		}
		if d.amt == 0 {
			di++
		}
	}
	return out
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
