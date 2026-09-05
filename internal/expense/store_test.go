package expense

import (
	"testing"

	"github.com/greboid/splitpayments/internal/testdb"
)

// Shares must land one-row-per-user carrying both sides. The old settle-up
// flow built a self-payment as two rows for the same user, and the upsert
// let the second (owed-only) row overwrite the first, losing the paid half
// and leaving a permanent negative balance.
func TestInsertMergesSharesForSameUser(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)

	e := &Expense{
		GroupID:     1,
		Description: "Payment from Shane to Shane",
		Category:    "payment",
		Date:        "2026-09-04",
		IsPayment:   true,
		SplitMode:   SplitEven,
		CreatedBy:   1,
		Shares: []Share{
			{UserID: 1, Paid: 1000},
			{UserID: 1, Owed: 1000},
		},
	}
	if err := s.Insert(e); err != nil {
		t.Fatal(err)
	}
	got, err := s.ByID(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shares) != 1 {
		t.Fatalf("share rows = %d (%+v), want 1 merged row", len(got.Shares), got.Shares)
	}
	if sh := got.Shares[0]; sh.Paid != 1000 || sh.Owed != 1000 {
		t.Fatalf("merged share = paid %d owed %d, want paid 1000 owed 1000", sh.Paid, sh.Owed)
	}

	// The same merge applies when an update rewrites the shares.
	got.Shares = []Share{
		{UserID: 1, Paid: 500},
		{UserID: 1, Owed: 500},
		{UserID: 2, Owed: 500},
	}
	if err := s.Update(&got); err != nil {
		t.Fatal(err)
	}
	got, err = s.ByID(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Shares) != 2 {
		t.Fatalf("share rows after update = %d (%+v), want 2", len(got.Shares), got.Shares)
	}
	if sh := got.Shares[0]; sh.UserID != 1 || sh.Paid != 500 || sh.Owed != 500 {
		t.Fatalf("user 1 share after update = paid %d owed %d, want paid 500 owed 500", sh.Paid, sh.Owed)
	}
}
