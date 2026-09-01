package expense

// Form parsing and round-tripping. The wire format is plain urlencoded
// fields, authored by expense-form.js (and workable without JS in the even/
// single-payer case):
//
//	description, date, notes, category, split_mode
//	payer_id_<i> / payer_amount_<i>     one or more payer rows
//	participant_<uid>                   even mode checkboxes
//	exact_<uid>                         exact mode amounts
//	percent_<uid>                       percent mode values (e.g. "33.33")
//	shares_<uid>                        shares mode weights
//	item_desc_<i> / item_amount_<i>     itemized rows
//	item_people_<i>                     repeated "all" or user ids
//	receipt_file                        <uuid>.<ext> of a previously scanned receipt

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/greboid/splitpayments/internal/money"
)

type FormPayer struct {
	UserID int64
	Amount string
}

type FormItem struct {
	Description string
	Amount      string
	People      []string // "all" or user ids
}

// amountValue parses the item's amount string (0 when blank/invalid; the
// caller validates via ToSplitInput first).
func (fi FormItem) amountValue() int64 {
	v, _ := money.Parse(fi.Amount)
	return v
}

// assignees resolves the People list to concrete member ids; "all" means
// every member. Returns an error message when an id is unparseable.
func (fi FormItem) assignees(memberIDs []int64) ([]int64, error) {
	for _, p := range fi.People {
		if p == "all" {
			return append([]int64{}, memberIDs...), nil
		}
	}
	var out []int64
	for _, p := range fi.People {
		uid, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, errors.New("invalid item assignment")
		}
		out = append(out, uid)
	}
	return out, nil
}

// ExpenseForm round-trips the expense form: rendered on GET, rebuilt from
// the request on validation failure, and convertible to a SplitInput.
type ExpenseForm struct {
	Description  string
	Notes        string
	Category     string
	Date         string
	Mode         string
	Payers       []FormPayer
	Participants map[int64]bool
	Exact        map[int64]string
	Percent      map[int64]string
	Shares       map[int64]string
	Items        []FormItem
	ReceiptFile  string
}

// blankForm builds the initial form state for the "new expense" page.
func BlankForm(memberIDs []int64, currentUser int64, today string) ExpenseForm {
	f := ExpenseForm{
		Mode:         SplitEven,
		Date:         today,
		Category:     "general",
		Participants: map[int64]bool{},
		Exact:        map[int64]string{},
		Percent:      map[int64]string{},
		Shares:       map[int64]string{},
	}
	f.Payers = []FormPayer{{UserID: currentUser}}
	// Participants start unticked: who to split between is an explicit choice.
	return f
}

// ParseExpenseForm reads the form body into an ExpenseForm. Amount strings
// are kept verbatim so the page can redisplay them on validation errors.
func ParseExpenseForm(form url.Values, memberIDs []int64) ExpenseForm {
	f := ExpenseForm{
		Description:  strings.TrimSpace(form.Get("description")),
		Notes:        strings.TrimSpace(form.Get("notes")),
		Category:     form.Get("category"),
		Date:         strings.TrimSpace(form.Get("date")),
		Mode:         form.Get("split_mode"),
		ReceiptFile:  strings.TrimSpace(form.Get("receipt_file")),
		Participants: map[int64]bool{},
		Exact:        map[int64]string{},
		Percent:      map[int64]string{},
		Shares:       map[int64]string{},
	}
	if f.Mode == "" {
		f.Mode = SplitEven
	}
	if !validCategory(f.Category) {
		f.Category = "general"
	}

	for i := 0; ; i++ {
		ids := form[fmt.Sprintf("payer_id_%d", i)]
		if len(ids) == 0 {
			break
		}
		uid, err := strconv.ParseInt(ids[0], 10, 64)
		if err != nil {
			break
		}
		f.Payers = append(f.Payers, FormPayer{UserID: uid, Amount: strings.TrimSpace(form.Get(fmt.Sprintf("payer_amount_%d", i)))})
	}

	for _, id := range memberIDs {
		key := strconv.FormatInt(id, 10)
		if form.Get("participant_"+key) != "" {
			f.Participants[id] = true
		}
		if v := strings.TrimSpace(form.Get("exact_" + key)); v != "" {
			f.Exact[id] = v
		}
		if v := strings.TrimSpace(form.Get("percent_" + key)); v != "" {
			f.Percent[id] = v
		}
		if v := strings.TrimSpace(form.Get("shares_" + key)); v != "" {
			f.Shares[id] = v
		}
	}

	for i := 0; ; i++ {
		desc := form.Get(fmt.Sprintf("item_desc_%d", i))
		amount := form.Get(fmt.Sprintf("item_amount_%d", i))
		people := form[fmt.Sprintf("item_people_%d", i)]
		if desc == "" && amount == "" && len(people) == 0 {
			break
		}
		f.Items = append(f.Items, FormItem{
			Description: strings.TrimSpace(desc),
			Amount:      strings.TrimSpace(amount),
			People:      people,
		})
	}
	return f
}

// ToSplitInput parses the amount strings into a SplitInput for the balance
// computation, returning a user-facing error message on bad input.
func (f *ExpenseForm) ToSplitInput(memberIDs []int64) (SplitInput, string) {
	in := SplitInput{
		Mode:         f.Mode,
		MemberIDs:    memberIDs,
		Exact:        map[int64]int64{},
		Percent:      map[int64]int64{},
		SharesWeight: map[int64]int64{},
	}
	if f.Date == "" {
		return in, "Please pick a date."
	}
	if _, err := time.Parse("2006-01-02", f.Date); err != nil {
		return in, "The date must be in YYYY-MM-DD format."
	}
	if f.Description == "" {
		return in, "Please describe the expense."
	}

	for _, p := range f.Payers {
		amt, msg := parseAmount(p.Amount, true)
		if msg != "" {
			return in, "Paid amount: " + msg
		}
		in.Payers = append(in.Payers, PayerInput{UserID: p.UserID, Amount: amt})
	}

	for _, id := range memberIDs {
		if f.Participants[id] {
			in.Participants = append(in.Participants, id)
		}
		if v, ok := f.Exact[id]; ok {
			amt, msg := parseAmount(v, false)
			if msg != "" {
				return in, "Exact share: " + msg
			}
			in.Exact[id] = amt
		}
		if v, ok := f.Percent[id]; ok {
			bp, err := money.Parse(v)
			if err != nil || bp < 0 {
				return in, "Percentages must be numbers like 33.33."
			}
			in.Percent[id] = bp
		}
		if v, ok := f.Shares[id]; ok {
			w, err := strconv.ParseInt(v, 10, 64)
			if err != nil || w < 0 {
				return in, "Share counts must be whole numbers."
			}
			in.SharesWeight[id] = w
		}
	}

	for _, it := range f.Items {
		item := ItemInput{Description: it.Description}
		if it.Description == "" {
			return in, "Every item needs a description."
		}
		amt, msg := parseAmount(it.Amount, false)
		if msg != "" {
			return in, "Item amount: " + msg
		}
		item.Amount = amt
		for _, p := range it.People {
			if p == "all" {
				item.AssignAll = true
				continue
			}
			uid, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return in, "Invalid item assignment."
			}
			item.Assignees = append(item.Assignees, uid)
		}
		if item.AssignAll {
			item.Assignees = nil
		}
		in.Items = append(in.Items, item)
	}

	return in, ""
}

// parseAmount parses a money string; blankOk allows empty to mean zero.
func parseAmount(s string, blankOk bool) (int64, string) {
	if s == "" {
		if blankOk {
			return 0, ""
		}
		return 0, ""
	}
	v, err := money.Parse(s)
	if err != nil || v < 0 {
		return 0, "not a valid amount"
	}
	return v, ""
}

// Stored receipt names are "<uuid>.<ext>", as returned by receipt.Store.Save.
var receiptFileRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.(jpg|png|webp)$`)

// ValidReceiptFile reports whether s is a plausible stored receipt name.
func ValidReceiptFile(s string) bool { return receiptFileRe.MatchString(s) }

func validCategory(c string) bool {
	for _, cat := range Categories {
		if cat == c {
			return true
		}
	}
	return false
}
