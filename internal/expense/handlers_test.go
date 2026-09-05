package expense

import "testing"

func TestChangeSummary(t *testing.T) {
	names := map[int64]string{1: "Alice", 2: "Bob", 3: "Carol"}

	regular := func() Expense {
		return Expense{
			Description: "Dinner", Date: "2026-09-01", Category: "dining",
			Shares: []Share{
				{UserID: 1, Paid: 3000},
				{UserID: 1, Owed: 1500}, {UserID: 2, Owed: 1500},
			},
		}
	}

	t.Run("unchanged", func(t *testing.T) {
		if s := changeSummary(regular(), regular(), names, "£"); s != "" {
			t.Errorf("changeSummary = %q, want empty", s)
		}
	})

	t.Run("date and amount", func(t *testing.T) {
		newE := regular()
		newE.Date = "2026-09-05"
		for i := range newE.Shares {
			newE.Shares[i].Paid *= 6 / 5
			newE.Shares[i].Owed *= 6 / 5
		}
		newE.Shares[0].Paid = 3600
		got := changeSummary(regular(), newE, names, "£")
		want := "date → 2026-09-05, amount £30.00 → £36.00"
		if got != want {
			t.Errorf("changeSummary = %q, want %q", got, want)
		}
	})

	t.Run("rename and notes", func(t *testing.T) {
		newE := regular()
		newE.Description = "Dinner party"
		newE.Notes = "incl. drinks"
		got := changeSummary(regular(), newE, names, "£")
		want := `renamed from “Dinner”, notes changed`
		if got != want {
			t.Errorf("changeSummary = %q, want %q", got, want)
		}
	})

	t.Run("payer swap", func(t *testing.T) {
		newE := regular()
		newE.Shares[0] = Share{UserID: 2, Paid: 3000, Owed: 1500}
		got := changeSummary(regular(), newE, names, "£")
		want := "payer Alice → Bob, split changed"
		if got != want {
			t.Errorf("changeSummary = %q, want %q", got, want)
		}
	})

	t.Run("same total resplit", func(t *testing.T) {
		newE := regular()
		newE.Shares[1] = Share{UserID: 1, Owed: 1000}
		newE.Shares[2] = Share{UserID: 2, Owed: 2000}
		got := changeSummary(regular(), newE, names, "£")
		want := "split changed"
		if got != want {
			t.Errorf("changeSummary = %q, want %q", got, want)
		}
	})

	t.Run("payment recipient and amount", func(t *testing.T) {
		oldPay := Expense{
			Description: "Payment from Bob to Alice", IsPayment: true,
			Shares: []Share{{UserID: 2, Paid: 1500}, {UserID: 1, Owed: 1500}},
		}
		newPay := Expense{
			Description: "Payment from Bob to Alice", IsPayment: true,
			Shares: []Share{{UserID: 2, Paid: 2000}, {UserID: 2, Owed: 2000}},
		}
		got := changeSummary(oldPay, newPay, names, "£")
		want := "amount £15.00 → £20.00, recipient Alice → Bob"
		if got != want {
			t.Errorf("changeSummary = %q, want %q", got, want)
		}
	})
}

func TestIsValidPaymentShape(t *testing.T) {
	cases := []struct {
		name   string
		shares []Share
		want   bool
	}{
		{"payment", []Share{{UserID: 2, Paid: 1500}, {UserID: 1, Owed: 1500}}, true},
		{"self-payment merged", []Share{{UserID: 2, Paid: 1500, Owed: 1500}}, true},
		{"split", []Share{{UserID: 1, Paid: 3000, Owed: 1500}, {UserID: 2, Owed: 1500}}, false},
		{"halved payment", []Share{{UserID: 2, Paid: 3000, Owed: 1500}, {UserID: 1, Owed: 1500}}, false},
		{"no payer", []Share{{UserID: 1, Owed: 1500}}, false},
	}
	for _, tc := range cases {
		if got := (Expense{Shares: tc.shares}).IsValidPaymentShape(); got != tc.want {
			t.Errorf("%s: IsValidPaymentShape = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFormFromExpenseModePresentation(t *testing.T) {
	cases := []struct {
		name      string
		splitMode string
		isPayment bool
	}{
		{"payment", SplitEven, true},
		{"exact", SplitExact, false},
		{"percent", SplitPercent, false},
		{"shares", SplitShares, false},
		{"empty legacy value", "", false},
	}
	for _, c := range cases {
		e := Expense{
			SplitMode: c.splitMode, IsPayment: c.isPayment,
			Shares: []Share{
				{UserID: 1, Paid: 1000, Owed: 200},
				{UserID: 2, Owed: 600},
				{UserID: 3, Owed: 200},
			},
		}
		f := formFromExpense(e, []int64{1, 2, 3})
		if f.Mode != SplitExact {
			t.Errorf("%s: mode = %q, want %q", c.name, f.Mode, SplitExact)
		}
		if f.Exact[1] != "2.00" || f.Exact[2] != "6.00" || f.Exact[3] != "2.00" {
			t.Errorf("%s: exact = %v, want stored owed amounts", c.name, f.Exact)
		}
		if len(f.Payers) != 1 || f.Payers[0].UserID != 1 || f.Payers[0].Amount != "10.00" {
			t.Errorf("%s: payers = %v", c.name, f.Payers)
		}
	}
}

func TestFormFromExpenseEvenRoundTrip(t *testing.T) {
	e := Expense{
		SplitMode: SplitEven,
		Shares: []Share{
			{UserID: 1, Paid: 900, Owed: 300},
			{UserID: 2, Owed: 300},
			{UserID: 3, Owed: 300},
		},
	}
	f := formFromExpense(e, []int64{1, 2, 3})
	if f.Mode != SplitEven {
		t.Fatalf("mode = %q, want even", f.Mode)
	}
	for _, id := range []int64{1, 2, 3} {
		if !f.Participants[id] {
			t.Errorf("participant %d not ticked: %v", id, f.Participants)
		}
	}
	if len(f.Exact) != 0 {
		t.Errorf("exact = %v, want empty in even mode", f.Exact)
	}
}

func TestFormFromExpenseItemizedRoundTrip(t *testing.T) {
	e := Expense{
		SplitMode: SplitItemized,
		Shares:    []Share{{UserID: 1, Paid: 500}},
		Items: []Item{
			{Description: "Milk", Amount: 200, Assignees: []int64{1, 2}},
			{Description: "Bread", Amount: 300, Assignees: []int64{2}},
		},
	}
	f := formFromExpense(e, []int64{1, 2})
	if f.Mode != SplitItemized {
		t.Fatalf("mode = %q, want itemized", f.Mode)
	}
	if len(f.Items) != 2 || f.Items[0].Description != "Milk" || f.Items[0].Amount != "2.00" {
		t.Fatalf("items = %v", f.Items)
	}
	if len(f.Items[0].People) != 1 || f.Items[0].People[0] != "all" {
		t.Errorf("items[0].people = %v, want [all]", f.Items[0].People)
	}
	if len(f.Items[1].People) != 1 || f.Items[1].People[0] != "2" {
		t.Errorf("items[1].people = %v, want [2]", f.Items[1].People)
	}
}
