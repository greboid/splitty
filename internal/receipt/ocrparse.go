package receipt

// GLM-OCR and similar layout-parsing models transcribe a receipt as
// markdown — a header block, dot-leader or table rows for the items, and
// closing total/tax/payment lines — but do not follow extraction prompts,
// so the structure is recovered here with line-oriented heuristics.
// Whatever this misreads lands in the itemization editor, where the user
// can correct it before saving.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type lineKind int

const (
	kindItem lineKind = iota
	kindSubtotal
	kindTotal
	kindTax
	kindTip
	kindPayment
)

var (
	htmlTagRe = regexp.MustCompile(`(?i)<[^>]*>`)
	markupRe  = strings.NewReplacer("|", " ", "*", " ", "#", " ", "`", " ")
	leaderRe  = regexp.MustCompile(`[.·•_]{2,}`)
	// A trailing price like "13.45", "$1,234.56", "13,45" or "2.00-"
	// (accounting minus). Requiring two decimals keeps item counts,
	// quantities and phone numbers from being mistaken for prices.
	moneyRe = regexp.MustCompile(`-?(?:[$€£¥]\s*)?\d{1,3}(?:[,. ]\d{3})*[.,]\d{2}-?\s*$`)

	subtotalRe = regexp.MustCompile(`(?i)sub\s*-?\s*total`)
	totalRe    = regexp.MustCompile(`(?i)(?:grand\s+)?total|amount\s+due|balance\s+due`)
	taxRe      = regexp.MustCompile(`(?i)\b(?:sales\s+)?(?:tax|vat|gst|hst|pst|qst)\b`)
	tipRe      = regexp.MustCompile(`(?i)\b(?:tip|gratuity|service\s+charge)\b`)
	paymentRe  = regexp.MustCompile(`(?i)\b(?:cash|change|visa|master\s*card|mastercard|amex|american\s+express|debit|credit|contactless|tender|payment|paid|gift\s*card)\b`)
	headerRe   = regexp.MustCompile(`(?i)receipt|invoice`)

	isoDateRe   = regexp.MustCompile(`(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})`)
	slashDateRe = regexp.MustCompile(`\b(\d{1,2})[-/.](\d{1,2})[-/.](\d{2,4})\b`)
	// "30 Aug 2026" and "Aug 30 2026"
	dayMonthDateRe = regexp.MustCompile(`(?i)\b(\d{1,2})\s+(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?,?\s+(\d{4})\b`)
	monthDayDateRe = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2}),?\s+(\d{4})\b`)
	monthNumbers   = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
)

// parseReceiptMarkdown turns OCR'd receipt text into a ScanResult. It fails
// only when no prices could be read at all; partial reads are returned as-is.
func parseReceiptMarkdown(md string) (ScanResult, error) {
	var result ScanResult
	var items []Item
	var totals, taxes, tips []float64
	parsedAny := false

	for _, line := range strings.Split(md, "\n") {
		desc, amount, ok := splitLine(line)
		if !ok {
			continue
		}
		parsedAny = true
		switch kind := classifyLine(desc); kind {
		case kindItem:
			if hasLetter(desc) {
				items = append(items, Item{Description: desc, Amount: amount})
			}
		case kindSubtotal, kindTotal:
			// The grand total is printed after the subtotal, so the last
			// match wins.
			totals = append(totals, amount)
		case kindTax:
			taxes = append(taxes, amount)
		case kindTip:
			tips = append(tips, amount)
		case kindPayment:
			// Cash tendered, card payments and change are neither items
			// nor totals.
		}
	}

	if len(totals) > 0 {
		result.Total = totals[len(totals)-1]
	} else {
		for _, it := range items {
			result.Total += it.Amount
		}
	}
	for _, t := range taxes {
		result.Tax += t
	}
	for _, t := range tips {
		result.Tip += t
	}
	result.Items = items
	result.Merchant = findMerchant(md)
	result.Date = findDate(md)

	if !parsedAny {
		return result, errors.New("no prices found in the receipt text")
	}
	return result, nil
}

// splitLine extracts a trailing price and the text before it from one
// (already ordinary-looking) receipt line.
func splitLine(line string) (string, float64, bool) {
	line = normalizeLine(line)
	loc := moneyRe.FindStringIndex(line)
	if loc == nil {
		return "", 0, false
	}
	desc := strings.Trim(line[:loc[0]], " -$€£¥:.,")
	return desc, parseMoney(line[loc[0]:loc[1]]), true
}

// normalizeLine strips HTML (OCR models emit tables as HTML), markdown
// decorations, dot leaders and runs of whitespace.
func normalizeLine(line string) string {
	line = htmlTagRe.ReplaceAllString(line, " ")
	line = markupRe.Replace(line)
	line = leaderRe.ReplaceAllString(line, " ")
	return strings.Join(strings.Fields(line), " ")
}

func classifyLine(desc string) lineKind {
	switch {
	case subtotalRe.MatchString(desc):
		return kindSubtotal
	case taxRe.MatchString(desc):
		return kindTax
	case tipRe.MatchString(desc):
		return kindTip
	case totalRe.MatchString(desc):
		return kindTotal
	case paymentRe.MatchString(desc):
		return kindPayment
	}
	return kindItem
}

// findMerchant takes the first header-like line: anything before the first
// priced line that isn't a recognised keyword line.
func findMerchant(md string) string {
	for _, line := range strings.Split(md, "\n") {
		if _, _, ok := splitLine(line); ok {
			return "" // items have started; the header is over
		}
		norm := normalizeLine(line)
		if classifyLine(norm) != kindItem || headerRe.MatchString(norm) || countLetters(norm) < 3 {
			continue
		}
		return norm
	}
	return ""
}

// parseMoney handles the "$1,234.56", "1.234,56" and "13,45" styles.
func parseMoney(token string) float64 {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0
	}
	negative := strings.HasPrefix(token, "-") || strings.HasSuffix(token, "-")
	token = strings.Trim(token, "-$€£¥ ")

	lastComma, lastDot := strings.LastIndex(token, ","), strings.LastIndex(token, ".")
	switch {
	case lastComma >= 0 && lastDot >= 0:
		if lastComma > lastDot { // 1.234,56
			token = strings.ReplaceAll(token, ".", "")
			token = strings.ReplaceAll(token, ",", ".")
		} else { // 1,234.56
			token = strings.ReplaceAll(token, ",", "")
		}
	case lastComma >= 0: // 13,45 or 1,234
		if parts := strings.Split(token, ","); len(parts) == 2 && len(parts[1]) == 2 {
			token = strings.ReplaceAll(token, ",", ".")
		} else {
			token = strings.ReplaceAll(token, ",", "")
		}
	case lastDot >= 0: // 13.45 or 1.234
		if parts := strings.Split(token, "."); !(len(parts) == 2 && len(parts[1]) == 2) {
			token = strings.ReplaceAll(token, ".", "")
		}
	}

	n, err := strconv.ParseFloat(token, 64)
	if err != nil {
		return 0
	}
	if negative {
		n = -n
	}
	return n
}

// findDate returns the first date on the receipt as YYYY-MM-DD. Numeric
// day/month orders are disambiguated when possible; genuinely ambiguous
// dates are read day-first, as printed outside the US.
func findDate(md string) string {
	if m := isoDateRe.FindStringSubmatch(md); m != nil {
		if mo, d := atoi(m[2]), atoi(m[3]); validMonthDay(mo, d) {
			return fmt.Sprintf("%s-%02d-%02d", m[1], mo, d)
		}
	}
	if m := dayMonthDateRe.FindStringSubmatch(md); m != nil {
		if mo := monthNumber(m[2]); mo > 0 && atoi(m[1]) > 0 {
			return fmt.Sprintf("%s-%02d-%02d", m[3], mo, atoi(m[1]))
		}
	}
	if m := monthDayDateRe.FindStringSubmatch(md); m != nil {
		if mo := monthNumber(m[1]); mo > 0 && atoi(m[2]) > 0 {
			return fmt.Sprintf("%s-%02d-%02d", m[3], mo, atoi(m[2]))
		}
	}
	if m := slashDateRe.FindStringSubmatch(md); m != nil {
		year := m[3]
		if len(year) == 2 {
			year = "20" + year
		}
		a, b := atoi(m[1]), atoi(m[2])
		day, mo := a, b
		if mo > 12 && day <= 12 { // e.g. 08/30/2026
			day, mo = b, a
		}
		if validMonthDay(mo, day) {
			return fmt.Sprintf("%s-%02d-%02d", year, mo, day)
		}
	}
	return ""
}

func monthNumber(abbr string) int {
	for i, m := range monthNumbers {
		if abbr == m {
			return i + 1
		}
	}
	return 0
}

func validMonthDay(mo, day int) bool {
	return mo >= 1 && mo <= 12 && day >= 1 && day <= 31
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func hasLetter(s string) bool {
	return strings.IndexFunc(s, unicode.IsLetter) >= 0
}

func countLetters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}
