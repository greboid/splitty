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
	"math/big"
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
// total. The intermediate products (total×weight, and the weight sum) can
// exceed int64, so the arithmetic is carried out with math/big; every
// share is at most |total| and therefore always fits.
func AllocateByWeights(total Amount, weights []int64) []Amount {
	if len(weights) == 0 {
		return nil
	}
	var sum big.Int
	bw := make([]big.Int, len(weights))
	for i, w := range weights {
		if w < 0 {
			return nil
		}
		bw[i].SetInt64(w)
		sum.Add(&sum, &bw[i])
	}
	if sum.Sign() == 0 {
		return nil
	}
	t := new(big.Int).Abs(big.NewInt(total))
	out := make([]Amount, len(weights))
	// gap_i = t*w_i mod sum is exactly how far share i sits below its
	// proportional value; giving share i one more unit reduces its gap by
	// sum, so the gaps stay in [0, sum) throughout.
	gaps := make([]big.Int, len(weights))
	rem := new(big.Int).Set(t)
	var prod, q big.Int
	for i := range weights {
		prod.Mul(t, &bw[i])
		q.QuoRem(&prod, &sum, &gaps[i])
		out[i] = q.Int64() // ≤ t, always fits
		rem.Sub(rem, &q)
	}
	one := big.NewInt(1)
	for rem.Sign() > 0 {
		best := 0
		for i := 1; i < len(weights); i++ {
			if gaps[i].Cmp(&gaps[best]) > 0 {
				best = i
			}
		}
		out[best]++
		gaps[best].Sub(&gaps[best], &sum)
		rem.Sub(rem, one)
	}
	if total < 0 {
		for i := range out {
			out[i] = -out[i]
		}
	}
	return out
}
