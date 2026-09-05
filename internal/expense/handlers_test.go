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
