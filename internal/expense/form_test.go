package expense

// Tests for receipt_file handling: only names in the stored
// "<uuid>.<ext>" format survive buildExpense; anything else is dropped
// rather than stored.

import "testing"

func TestValidReceiptFile(t *testing.T) {
	valid := []string{
		"550e8400-e29b-41d4-a716-446655440000.png",
		"550e8400-e29b-41d4-a716-446655440000.jpg",
		"550e8400-e29b-41d4-a716-446655440000.webp",
	}
	for _, s := range valid {
		if !ValidReceiptFile(s) {
			t.Errorf("ValidReceiptFile(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		"550e8400-e29b-41d4-a716-446655440000", // missing extension
		"550e8400-e29b-41d4-a716-446655440000.gif",  // unsupported type
		"550e8400-e29b-41d4-a716-446655440000.pngx", // junk suffix
		"notes.txt", // not a receipt
		"../550e8400-e29b-41d4-a716-446655440000.png", // traversal
	}
	for _, s := range invalid {
		if ValidReceiptFile(s) {
			t.Errorf("ValidReceiptFile(%q) = true, want false", s)
		}
	}
}

func TestBuildExpenseKeepsScannedReceipt(t *testing.T) {
	h := &Handlers{}
	form := BlankForm([]int64{1}, 1, "2026-08-31")
	form.Description = "Coffee"
	form.Date = "2026-08-31"
	form.Payers = []FormPayer{{UserID: 1, Amount: "4.50"}}
	form.Participants[1] = true
	form.ReceiptFile = "550e8400-e29b-41d4-a716-446655440000.png"

	e, msg := h.buildExpense(&form, []int64{1}, 9, 1)
	if msg != "" {
		t.Fatalf("buildExpense: %s", msg)
	}
	if e.ReceiptFile != form.ReceiptFile {
		t.Errorf("receipt file %q, want %q", e.ReceiptFile, form.ReceiptFile)
	}
}

func TestBuildExpenseDropsJunkReceiptFile(t *testing.T) {
	h := &Handlers{}
	form := BlankForm([]int64{1}, 1, "2026-08-31")
	form.Description = "Coffee"
	form.Date = "2026-08-31"
	form.Payers = []FormPayer{{UserID: 1, Amount: "4.50"}}
	form.Participants[1] = true
	form.ReceiptFile = "../../etc/passwd"

	e, msg := h.buildExpense(&form, []int64{1}, 9, 1)
	if msg != "" {
		t.Fatalf("buildExpense: %s", msg)
	}
	if e.ReceiptFile != "" {
		t.Errorf("junk receipt file %q kept, want cleared", e.ReceiptFile)
	}
}
