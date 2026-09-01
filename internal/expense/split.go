package expense

import (
	"errors"
	"fmt"

	"github.com/greboid/splitpayments/internal/money"
)

// PayerInput is one payer row from the form.
type PayerInput struct {
	UserID int64
	Amount int64 // minor units, >= 0
}

// ItemInput is one itemized line from the form.
type ItemInput struct {
	Description string
	Amount      int64
	Assignees   []int64 // empty with AssignAll means "no one", which is invalid
	AssignAll   bool    // split among all members
}

// SplitInput is the validated-shape form payload for one expense, ready for
// owed-amount computation. The server never trusts client-computed shares;
// it recomputes owed values from this input for every mode.
type SplitInput struct {
	Mode         string
	MemberIDs    []int64 // all group members, the only valid user ids
	Payers       []PayerInput
	Participants []int64         // even mode
	Exact        map[int64]int64 // exact mode
	Percent      map[int64]int64 // percent mode, in basis points (33.33% = 3333)
	SharesWeight map[int64]int64 // shares mode
	Items        []ItemInput
}

var ErrSplit = errors.New("invalid split")

func errf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSplit, fmt.Sprintf(format, args...))
}

// Total returns the sum of payer amounts.
func (in *SplitInput) Total() int64 {
	var t int64
	for _, p := range in.Payers {
		t += p.Amount
	}
	return t
}

func (in *SplitInput) isMember(id int64) bool {
	for _, m := range in.MemberIDs {
		if m == id {
			return true
		}
	}
	return false
}

// ComputeOwed validates the input against its mode and returns the owed
// amount per participant. Returned maps contain only nonzero entries. The
// result always sums to the expense total exactly.
func (in *SplitInput) ComputeOwed() (map[int64]int64, error) {
	total := in.Total()
	if total <= 0 {
		return nil, errf("the amounts paid must add up to more than zero")
	}
	for _, p := range in.Payers {
		if p.Amount < 0 {
			return nil, errf("paid amounts cannot be negative")
		}
		if !in.isMember(p.UserID) {
			return nil, errf("a payer is not a member of this group")
		}
	}

	switch in.Mode {
	case SplitEven:
		if len(in.Participants) == 0 {
			return nil, errf("select at least one person to split between")
		}
		for _, u := range in.Participants {
			if !in.isMember(u) {
				return nil, errf("a participant is not a member of this group")
			}
		}
		return allocToMap(money.Allocate(total, len(in.Participants)), in.Participants), nil

	case SplitExact:
		owed := map[int64]int64{}
		var sum int64
		for uid, amt := range in.Exact {
			if !in.isMember(uid) {
				return nil, errf("a participant is not a member of this group")
			}
			if amt < 0 {
				return nil, errf("shares cannot be negative")
			}
			sum += amt
			if amt != 0 {
				owed[uid] = amt
			}
		}
		if sum != total {
			return nil, errf("the exact amounts add up to %s but the expense total is %s",
				money.Format(sum, ""), money.Format(total, ""))
		}
		return owed, nil

	case SplitPercent:
		owed := map[int64]int64{}
		ids := make([]int64, 0, len(in.Percent))
		weights := make([]int64, 0, len(in.Percent))
		var sum int64
		for uid, bp := range in.Percent {
			if !in.isMember(uid) {
				return nil, errf("a participant is not a member of this group")
			}
			if bp < 0 {
				return nil, errf("percentages cannot be negative")
			}
			sum += bp
			if bp != 0 {
				ids = append(ids, uid)
				weights = append(weights, bp)
			}
		}
		if sum != 10000 {
			return nil, errf("percentages add up to %s%%, not 100%%", basisPoints(sum))
		}
		for i, part := range money.AllocateByWeights(total, weights) {
			if part != 0 {
				owed[ids[i]] = part
			}
		}
		return owed, nil

	case SplitShares:
		owed := map[int64]int64{}
		ids := make([]int64, 0, len(in.SharesWeight))
		weights := make([]int64, 0, len(in.SharesWeight))
		for uid, w := range in.SharesWeight {
			if !in.isMember(uid) {
				return nil, errf("a participant is not a member of this group")
			}
			if w < 0 {
				return nil, errf("share counts cannot be negative")
			}
			if w != 0 {
				ids = append(ids, uid)
				weights = append(weights, w)
			}
		}
		if len(ids) == 0 {
			return nil, errf("give at least one person a share")
		}
		for i, part := range money.AllocateByWeights(total, weights) {
			if part != 0 {
				owed[ids[i]] = part
			}
		}
		return owed, nil

	case SplitItemized:
		if len(in.Items) == 0 {
			return nil, errf("add at least one item")
		}
		var subtotal int64
		perUser := map[int64]int64{} // item cost per user (exact, incl. rounding)
		for _, it := range in.Items {
			if it.Amount < 0 {
				return nil, errf("item amounts cannot be negative")
			}
			if it.AssignAll {
				it.Assignees = append([]int64{}, in.MemberIDs...)
			}
			if len(it.Assignees) == 0 {
				return nil, errf("assign every item to at least one person")
			}
			for _, u := range it.Assignees {
				if !in.isMember(u) {
					return nil, errf("an item is assigned to someone outside this group")
				}
			}
			subtotal += it.Amount
			for u, part := range itemAlloc(it.Amount, it.Assignees) {
				perUser[u] += part
			}
		}
		if subtotal != total {
			return nil, errf("items (%s) must equal the amount paid (%s)",
				money.Format(subtotal, ""), money.Format(total, ""))
		}
		return perUser, nil

	default:
		return nil, errf("unknown split mode %q", in.Mode)
	}
}

// itemAlloc splits one item's amount evenly among its assignees with any
// remainder going to the earliest ids (deterministic: assignees sorted).
func itemAlloc(amount int64, assignees []int64) map[int64]int64 {
	ids := append([]int64{}, assignees...)
	sortInt64s(ids)
	parts := money.Allocate(amount, len(ids))
	out := make(map[int64]int64, len(ids))
	for i, id := range ids {
		out[id] += parts[i]
	}
	return out
}

func allocToMap(parts []int64, ids []int64) map[int64]int64 {
	out := make(map[int64]int64, len(ids))
	for i, id := range ids {
		if parts[i] != 0 {
			out[id] += parts[i]
		}
	}
	return out
}

func sortInt64s(v []int64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func basisPoints(bp int64) string {
	sign := ""
	if bp < 0 {
		sign = "-"
		bp = -bp
	}
	return fmt.Sprintf("%s%d.%02d", sign, bp/100, bp%100)
}
