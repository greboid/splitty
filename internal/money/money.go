// Package money provides currency-neutral amount handling backed by an
// integer count of minor units (e.g. pence for GBP). Floating point is never
// used for money anywhere in the app.
//
// money.Amount is an alias of int64 so amounts flow through stores and
// templates without conversion ceremony; the name documents intent.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amount is a number of minor currency units (pence for GBP). It may be
// negative to represent a net debt.
type Amount = int64

// MaxAmount bounds parsed values slightly below MaxInt64/100 so arithmetic
// on pence stays comfortably in range.
const MaxAmount = math.MaxInt64 / 1000

var ErrInvalid = errors.New("invalid amount")

// Parse parses a user-supplied decimal string such as "12", "12.5" or "-3.99"
// into minor units. At most two decimal places are accepted.
func Parse(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalid)
	}
	s = strings.ReplaceAll(s, ",", "")
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	major, minor := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		major, minor = s[:i], s[i+1:]
	}
	if major == "" && minor == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	if len(minor) > 2 {
		return 0, fmt.Errorf("%w: %q has more than two decimal places", ErrInvalid, s)
	}
	for len(minor) < 2 {
		minor += "0"
	}
	var whole, frac int64
	var err error
	if major != "" {
		whole, err = strconv.ParseInt(major, 10, 64)
		if err != nil || whole < 0 {
			return 0, fmt.Errorf("%w: %q", ErrInvalid, s)
		}
	}
	frac, err = strconv.ParseInt(minor, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	if whole > MaxAmount {
		return 0, fmt.Errorf("%w: %q too large", ErrInvalid, s)
	}
	v := whole*100 + frac
	if neg {
		v = -v
	}
	return v, nil
}

// MustParse is Parse for hard-coded literals in tests and templates.
func MustParse(s string) Amount {
	a, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return a
}

// Format renders the amount prefixed with the given currency symbol, e.g.
// "£12.34" or "-£3.99". Negative amounts place the sign before the symbol.
func Format(v Amount, symbol string) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = -v
	}
	return fmt.Sprintf("%s%s%d.%02d", sign, symbol, v/100, v%100)
}

// FormatSigned renders with an explicit + for positive values, for balance
// lists.
func FormatSigned(v Amount, symbol string) string {
	if v > 0 {
		return "+" + Format(v, symbol)
	}
	return Format(v, symbol)
}

// Allocate divides total into n shares as evenly as the integer minor units
// allow, distributing the remainder one minor unit at a time to the earliest
// shares. The results always sum to exactly total. For n <= 0 returns nil.
func Allocate(total Amount, n int) []Amount {
	if n <= 0 {
		return nil
	}
	base := total / int64(n)
	rem := total % int64(n)
	out := make([]Amount, n)
	for i := range out {
		share := base
		if rem > 0 && int64(i) < rem {
			share++
		} else if rem < 0 && int64(i) < -rem {
			share--
		}
		out[i] = share
	}
	return out
}

// AllocateByWeights divides total across integer weights exactly, using the
// largest-remainder method: each share starts at the floor of its exact
// proportional value, and leftover minor units go one at a time to the most
// under-allocated share (ties broken by lowest index). Returns nil when the
// weights are empty, negative, or sum to zero. The results sum to exactly
// total.
func AllocateByWeights(total Amount, weights []int64) []Amount {
	if len(weights) == 0 {
		return nil
	}
	var sum int64
	for _, w := range weights {
		if w < 0 {
			return nil
		}
		sum += w
	}
	if sum == 0 {
		return nil
	}
	sign := int64(1)
	t := total
	if t < 0 {
		sign = -1
		t = -t
	}
	out := make([]Amount, len(weights))
	var allocated int64
	for i, w := range weights {
		out[i] = t * w / sum
		allocated += out[i]
	}
	for rem := t - allocated; rem > 0; rem-- {
		best, bestGap := -1, int64(-1)
		for i, w := range weights {
			if gap := t*w - out[i]*sum; gap > bestGap {
				best, bestGap = i, gap
			}
		}
		out[best]++
	}
	for i := range out {
		out[i] *= sign
	}
	return out
}
