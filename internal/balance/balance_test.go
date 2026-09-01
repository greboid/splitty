package balance

import (
	"reflect"
	"testing"
)

func exp(id int64, shares ...Share) Expense {
	for i := range shares {
		shares[i].ExpenseID = id
	}
	return Expense{ID: id, Shares: shares}
}

func TestNet(t *testing.T) {
	expenses := []Expense{
		exp(1, Share{UserID: 1, Paid: 3000, Owed: 1000}, Share{UserID: 2, Paid: 0, Owed: 1000}, Share{UserID: 3, Paid: 0, Owed: 1000}),
		exp(2, Share{UserID: 2, Paid: 500}, Share{UserID: 2, Paid: 0, Owed: 500}),
	}
	net := Net(expenses)
	want := map[int64]int64{1: 2000, 2: -1000 + 500 - 500, 3: -1000}
	if !reflect.DeepEqual(net, want) {
		t.Errorf("Net = %v, want %v", net, want)
	}
}

func TestPairwiseSimple(t *testing.T) {
	expenses := []Expense{
		exp(1, Share{UserID: 1, Paid: 3000, Owed: 1000}, Share{UserID: 2, Paid: 0, Owed: 1000}, Share{UserID: 3, Paid: 0, Owed: 1000}),
	}
	got := Pairwise(expenses)
	want := []Debt{{From: 2, To: 1, Amount: 1000}, {From: 3, To: 1, Amount: 1000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pairwise = %v, want %v", got, want)
	}
}

func TestPairwiseMultiplePayersProportional(t *testing.T) {
	// A pays 70, B pays 30; C owes 100 → C owes A 70, B 30.
	expenses := []Expense{
		exp(1,
			Share{UserID: 1, Paid: 7000, Owed: 0},
			Share{UserID: 2, Paid: 3000, Owed: 0},
			Share{UserID: 3, Paid: 0, Owed: 10000}),
	}
	got := Pairwise(expenses)
	want := []Debt{{From: 3, To: 1, Amount: 7000}, {From: 3, To: 2, Amount: 3000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pairwise = %v, want %v", got, want)
	}
}

func TestPairwiseRoundingSumsExactly(t *testing.T) {
	// A pays 1, B pays 2; C owes 1p → split 1/3,2/3 of 1p = 0p and 1p; total preserved.
	expenses := []Expense{
		exp(1,
			Share{UserID: 1, Paid: 1, Owed: 0},
			Share{UserID: 2, Paid: 2, Owed: 0},
			Share{UserID: 3, Paid: 0, Owed: 1}),
	}
	var sum int64
	for _, d := range Pairwise(expenses) {
		sum += d.Amount
	}
	if sum != 1 {
		t.Errorf("rounded pairwise debts sum to %d, want 1", sum)
	}
}

func TestPairwiseCancelsOpposing(t *testing.T) {
	// A pays for B (10) and B pays for A (4): net A is owed 6.
	expenses := []Expense{
		exp(1, Share{UserID: 1, Paid: 1000, Owed: 0}, Share{UserID: 2, Paid: 0, Owed: 1000}),
		exp(2, Share{UserID: 2, Paid: 400, Owed: 0}, Share{UserID: 1, Paid: 0, Owed: 400}),
	}
	got := Pairwise(expenses)
	want := []Debt{{From: 2, To: 1, Amount: 600}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pairwise = %v, want %v", got, want)
	}
}

func TestPairwisePayment(t *testing.T) {
	// Settle-up: A "pays" 500 to B (A paid, B owed).
	expenses := []Expense{
		{ID: 1, IsPayment: true, Shares: []Share{Share{UserID: 1, Paid: 500}, Share{UserID: 2, Owed: 500}}},
	}
	got := Pairwise(expenses)
	want := []Debt{{From: 2, To: 1, Amount: 500}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pairwise = %v, want %v", got, want)
	}
}

func TestSimplifyCancelsCircular(t *testing.T) {
	// Circular: A owes B 10, B owes C 10, C owes A 10 → nobody owes anything.
	expenses := []Expense{
		exp(1, Share{UserID: 1, Paid: 1000, Owed: 0}, Share{UserID: 2, Paid: 0, Owed: 1000}),
		exp(2, Share{UserID: 2, Paid: 1000, Owed: 0}, Share{UserID: 3, Paid: 0, Owed: 1000}),
		exp(3, Share{UserID: 3, Paid: 1000, Owed: 0}, Share{UserID: 1, Paid: 0, Owed: 1000}),
	}
	got := Simplify(expenses)
	if len(got) != 0 {
		t.Errorf("circular debts should cancel, got %v", got)
	}
}

func TestSimplifyMinTransfers(t *testing.T) {
	// A owes 30 total, B and C are owed 15 each → one transfer A→B 15, A→C 15.
	expenses := []Expense{
		exp(1, Share{UserID: 1, Paid: 0, Owed: 3000}, Share{UserID: 2, Paid: 1500, Owed: 0}, Share{UserID: 3, Paid: 1500, Owed: 0}),
	}
	got := Simplify(expenses)
	want := []Debt{{From: 1, To: 2, Amount: 1500}, {From: 1, To: 3, Amount: 1500}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Simplify = %v, want %v", got, want)
	}
}

func TestSimplifyPreservesTotals(t *testing.T) {
	expenses := []Expense{
		exp(1, Share{UserID: 1, Paid: 12005, Owed: 4000}, Share{UserID: 2, Paid: 0, Owed: 4001}, Share{UserID: 3, Paid: 0, Owed: 4004}),
		exp(2, Share{UserID: 2, Paid: 999, Owed: 0}, Share{UserID: 1, Paid: 0, Owed: 999}),
		exp(3, Share{UserID: 3, Paid: 501, Owed: 250}, Share{UserID: 2, Paid: 0, Owed: 251}),
	}
	before := Net(expenses)
	for _, d := range Simplify(expenses) {
		// Applying a transfer is a payment: the sender's paid rises, the
		// recipient takes on the amount as owed.
		before[d.From] += d.Amount
		before[d.To] -= d.Amount
	}
	for u, v := range before {
		if v != 0 {
			t.Errorf("user %d left with %d after simplification", u, v)
		}
	}
}
