package expense

import (
	"errors"
	"testing"
)

func members(n ...int64) []int64 { return n }

func TestComputeOwedEven(t *testing.T) {
	in := &SplitInput{
		Mode:         SplitEven,
		MemberIDs:    members(1, 2, 3),
		Payers:       []PayerInput{{1, 100}},
		Participants: []int64{1, 2, 3},
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		t.Fatal(err)
	}
	if owed[1] != 34 || owed[2] != 33 || owed[3] != 33 {
		t.Errorf("owed = %v", owed)
	}
	var sum int64
	for _, v := range owed {
		sum += v
	}
	if sum != 100 {
		t.Errorf("sum = %d", sum)
	}
}

func TestComputeOwedEvenTwoParticipants(t *testing.T) {
	in := &SplitInput{
		Mode: SplitEven, MemberIDs: members(1, 2),
		Payers:       []PayerInput{{2, 999}},
		Participants: []int64{1, 2},
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		t.Fatal(err)
	}
	if owed[1] != 500 || owed[2] != 499 {
		t.Errorf("owed = %v", owed)
	}
}

func TestComputeOwedExact(t *testing.T) {
	in := &SplitInput{
		Mode: SplitExact, MemberIDs: members(1, 2),
		Payers: []PayerInput{{1, 2500}},
		Exact:  map[int64]int64{1: 1500, 2: 1000},
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		t.Fatal(err)
	}
	if owed[1] != 1500 || owed[2] != 1000 {
		t.Errorf("owed = %v", owed)
	}

	in.Exact[2] = 999
	if _, err := in.ComputeOwed(); !errors.Is(err, ErrSplit) {
		t.Errorf("mismatched exact sum should fail, got %v", err)
	}
}

func TestComputeOwedPercent(t *testing.T) {
	in := &SplitInput{
		Mode: SplitPercent, MemberIDs: members(1, 2, 3),
		Payers:  []PayerInput{{1, 10000}},
		Percent: map[int64]int64{1: 3333, 2: 3333, 3: 3334},
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, v := range owed {
		sum += v
	}
	if sum != 10000 {
		t.Errorf("sum = %d, want 10000", sum)
	}

	bad := &SplitInput{
		Mode: SplitPercent, MemberIDs: members(1, 2),
		Payers: []PayerInput{{1, 100}}, Percent: map[int64]int64{1: 5000, 2: 4000},
	}
	if _, err := bad.ComputeOwed(); err == nil {
		t.Error("90% should fail")
	}
}

func TestComputeOwedShares(t *testing.T) {
	in := &SplitInput{
		Mode: SplitShares, MemberIDs: members(1, 2, 3),
		Payers:       []PayerInput{{1, 600}},
		SharesWeight: map[int64]int64{1: 1, 2: 2, 3: 3},
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		t.Fatal(err)
	}
	if owed[1] != 100 || owed[2] != 200 || owed[3] != 300 {
		t.Errorf("owed = %v", owed)
	}
}

func TestComputeOwedItemized(t *testing.T) {
	in := &SplitInput{
		Mode: SplitItemized, MemberIDs: members(1, 2),
		Payers: []PayerInput{{1, 2000}},
		Items: []ItemInput{
			{Description: "Pizza", Amount: 1000, Assignees: []int64{1, 2}},
			{Description: "Salad", Amount: 1000, Assignees: []int64{1}},
		},
	}
	owed, err := in.ComputeOwed()
	if err != nil {
		t.Fatal(err)
	}
	// Items: user1 = 500+1000 = 1500, user2 = 500.
	if owed[1] != 1500 || owed[2] != 500 {
		t.Errorf("owed = %v (want 1:1500 2:500)", owed)
	}
}

func TestComputeOwedItemizedValidation(t *testing.T) {
	base := func() *SplitInput {
		return &SplitInput{
			Mode: SplitItemized, MemberIDs: members(1, 2),
			Payers: []PayerInput{{1, 1000}},
			Items:  []ItemInput{{Amount: 1000, Assignees: []int64{1, 2}}},
		}
	}
	noAssignee := base()
	noAssignee.Items[0].Assignees = nil
	if _, err := noAssignee.ComputeOwed(); err == nil {
		t.Error("unassigned item should fail")
	}
	stray := base()
	stray.Items[0].Assignees = []int64{9}
	if _, err := stray.ComputeOwed(); err == nil {
		t.Error("outside assignee should fail")
	}
	wrongTotal := base()
	wrongTotal.Items[0].Amount = 1050
	if _, err := wrongTotal.ComputeOwed(); err == nil {
		t.Error("items != paid should fail")
	}
	noItems := base()
	noItems.Items = nil
	if _, err := noItems.ComputeOwed(); err == nil {
		t.Error("no items should fail")
	}
}

func TestComputeOwedRejectsStrangers(t *testing.T) {
	in := &SplitInput{
		Mode: SplitEven, MemberIDs: members(1, 2),
		Payers: []PayerInput{{3, 100}},
	}
	if _, err := in.ComputeOwed(); err == nil {
		t.Error("payer outside group should fail")
	}
}

func TestComputeOwedRequiresPositiveTotal(t *testing.T) {
	in := &SplitInput{Mode: SplitEven, MemberIDs: members(1), Payers: []PayerInput{{1, 0}}}
	if _, err := in.ComputeOwed(); err == nil {
		t.Error("zero total should fail")
	}
}

// buildExpense is in handlers.go; these tests pin the share-assembly
// invariant that matters for balances: Σ paid == Σ owed for every expense,
// and a payer who is also a participant keeps their owed amount.
func TestBuildExpenseEvenWithPayerParticipant(t *testing.T) {
	h := &Handlers{}
	form := BlankForm([]int64{1, 2, 3}, 1, "2026-08-30")
	form.Description = "Groceries"
	form.Date = "2026-08-30"
	form.Payers = []FormPayer{{UserID: 1, Amount: "30.00"}}
	for id := range []int64{1, 2, 3} {
		form.Participants[int64(id)] = true
	}

	e, msg := h.buildExpense(&form, []int64{1, 2, 3}, 9, 1)
	if msg != "" {
		t.Fatalf("buildExpense: %s", msg)
	}
	var paidSum, owedSum int64
	for _, sh := range e.Shares {
		paidSum += sh.Paid
		owedSum += sh.Owed
		if sh.UserID == 1 && sh.Paid == 3000 && sh.Owed == 0 {
			t.Error("payer-participant lost their owed amount")
		}
	}
	if paidSum != owedSum {
		t.Errorf("Σ paid %d != Σ owed %d", paidSum, owedSum)
	}
	if paidSum != 3000 {
		t.Errorf("total %d, want 3000", paidSum)
	}
}

func TestBuildExpenseMultiPayerExact(t *testing.T) {
	h := &Handlers{}
	form := BlankForm([]int64{1, 2}, 1, "2026-08-30")
	form.Description = "Gift"
	form.Date = "2026-08-30"
	form.Mode = SplitExact
	form.Payers = []FormPayer{{UserID: 1, Amount: "40.00"}, {UserID: 2, Amount: "10.00"}}
	form.Exact = map[int64]string{1: "25.00", 2: "25.00"}

	e, msg := h.buildExpense(&form, []int64{1, 2}, 9, 1)
	if msg != "" {
		t.Fatalf("buildExpense: %s", msg)
	}
	var paidSum, owedSum int64
	for _, sh := range e.Shares {
		paidSum += sh.Paid
		owedSum += sh.Owed
	}
	if paidSum != 5000 || owedSum != 5000 {
		t.Errorf("paid %d / owed %d, want 5000/5000", paidSum, owedSum)
	}
}
