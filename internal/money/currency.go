// Package money represents monetary amounts as an exact integer count of a
// currency's minor units. No monetary value is ever represented, transported,
// or stored as a binary floating-point number.
//
// Arithmetic that is not exact in integers - applying a percentage rate, for
// example - is performed in arbitrary-precision decimal and rounded to whole
// minor units at the boundary.
package money

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrUnsupportedCurrency = errors.New("money: unsupported currency")
	ErrCurrencyMismatch    = errors.New("money: currency mismatch")

	// Excess precision is rejected rather than silently truncated: a caller
	// submitting 12.555 USD has made an error, and quietly charging them 12.55
	// or 12.56 would hide it.
	ErrPrecisionExceeded = errors.New("money: amount has more precision than the currency allows")
)

// Currency is an ISO 4217 currency together with its exponent - the number of
// decimal places occupied by its minor unit.
//
// The exponent is carried per currency because ISO 4217 does not assign a
// uniform one: JPY is 0, USD and GEL are 2, KWD is 3, and a handful are 4.
type Currency struct {
	Code     string `json:"code"`
	Exponent int32  `json:"exponent"`
}

func (c Currency) String() string { return c.Code }

// IsZero reports whether c is the zero Currency, which denominates nothing.
func (c Currency) IsZero() bool { return c.Code == "" }

// supported is the set of currencies this service accepts. Adding one is a
// matter of adding its entry; nothing else assumes a particular exponent.
var supported = map[string]Currency{
	"USD": {Code: "USD", Exponent: 2},
	"GEL": {Code: "GEL", Exponent: 2},
}

// Lookup resolves an ISO 4217 code, matched case-insensitively after trimming.
func Lookup(code string) (Currency, error) {
	c, ok := supported[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnsupportedCurrency, code)
	}
	return c, nil
}

// MustLookup is Lookup for currencies known at compile time. It panics on an
// unsupported code.
func MustLookup(code string) Currency {
	c, err := Lookup(code)
	if err != nil {
		panic(err)
	}
	return c
}

// Supported returns the supported ISO 4217 codes in sorted order.
func Supported() []string {
	codes := make([]string, 0, len(supported))
	for code := range supported {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}
