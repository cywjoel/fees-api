package money

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// Money is an exact monetary amount: a count of a currency's minor units,
// together with that currency.
//
// The fields are unexported so that every Money is built through a constructor
// that establishes its currency, rather than a zero value silently denominating
// nothing.
type Money struct {
	minorUnits int64
	currency   Currency
}

func New(minorUnits int64, c Currency) Money {
	return Money{minorUnits: minorUnits, currency: c}
}

func Zero(c Currency) Money { return Money{minorUnits: 0, currency: c} }

func (m Money) MinorUnits() int64  { return m.minorUnits }
func (m Money) Currency() Currency { return m.currency }
func (m Money) IsZero() bool       { return m.minorUnits == 0 }

func (m Money) Equal(o Money) bool {
	return m.minorUnits == o.minorUnits && m.currency.Code == o.currency.Code
}

// Add returns m+o, or ErrCurrencyMismatch if the two are denominated
// differently. Exact integer arithmetic: no error accumulates however many
// amounts are summed.
func (m Money) Add(o Money) (Money, error) {
	if m.currency.Code != o.currency.Code {
		return Money{}, fmt.Errorf("%w: cannot add %s to %s", ErrCurrencyMismatch, o.currency.Code, m.currency.Code)
	}
	return Money{minorUnits: m.minorUnits + o.minorUnits, currency: m.currency}, nil
}

func (m Money) Neg() Money {
	return Money{minorUnits: -m.minorUnits, currency: m.currency}
}

// Sum totals amounts in c. Every amount must be denominated in c; an empty sum
// is the zero amount in c.
func Sum(c Currency, amounts ...Money) (Money, error) {
	total := Zero(c)
	for i, a := range amounts {
		next, err := total.Add(a)
		if err != nil {
			return Money{}, fmt.Errorf("summing amount %d: %w", i, err)
		}
		total = next
	}
	return total, nil
}

// Decimal returns the amount in major units - 1255 minor units of USD becomes
// 12.55 - by scaling an integer, never through a floating-point representation.
func (m Money) Decimal() decimal.Decimal {
	return decimal.New(m.minorUnits, -m.currency.Exponent)
}

func (m Money) String() string {
	if m.currency.IsZero() {
		return fmt.Sprintf("%d (no currency)", m.minorUnits)
	}
	return fmt.Sprintf("%s %s", m.Decimal().StringFixed(m.currency.Exponent), m.currency.Code)
}

// FromMinorUnitsDecimal rounds a fractional quantity of minor units to a whole
// number of them, half-up: ties round away from zero.
//
// The single place the platform's rounding policy is expressed. Half-up is chosen
// for explicability - the rule fits in one sentence to a customer disputing a
// line item - knowing the trade-off: every tie resolves in the biller's favour.
func FromMinorUnitsDecimal(d decimal.Decimal, c Currency) Money {
	return Money{minorUnits: d.Round(0).IntPart(), currency: c}
}

// FromMajorUnitsDecimal converts a major-unit amount to Money, rounding half-up.
// 12.554 USD becomes 1255 minor units; 12.555 becomes 1256.
func FromMajorUnitsDecimal(d decimal.Decimal, c Currency) Money {
	return FromMinorUnitsDecimal(d.Shift(c.Exponent), c)
}

// ApplyRate returns base multiplied by rate, rounded to whole minor units
// half-up. The rate carries the precision, the product is rounded once, and the
// rounded value is both what is stored and what is charged.
func ApplyRate(base Money, rate decimal.Decimal) Money {
	product := decimal.New(base.minorUnits, 0).Mul(rate)
	return FromMinorUnitsDecimal(product, base.currency)
}

// ParseExact parses a major-unit decimal string such as "12.55", rejecting any
// amount carrying more decimal places than c permits.
//
// Excess precision is an error rather than something to round away: rounding
// belongs to rate derivation, not to an amount a caller stated explicitly.
func ParseExact(s string, c Currency) (Money, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Money{}, fmt.Errorf("money: parsing %q: %w", s, err)
	}
	if -d.Exponent() > c.Exponent {
		return Money{}, fmt.Errorf("%w: %s has more than %d decimal places for %s",
			ErrPrecisionExceeded, s, c.Exponent, c.Code)
	}
	shifted := d.Shift(c.Exponent)
	if !shifted.IsInteger() {
		return Money{}, fmt.Errorf("%w: %s is not a whole number of %s minor units",
			ErrPrecisionExceeded, s, c.Code)
	}
	return Money{minorUnits: shifted.IntPart(), currency: c}, nil
}

// moneyJSON is the wire form of an amount.
//
// The major-unit amount is carried as a JSON *string*, never a JSON number. A
// number would invite every consumer's parser to route the value through an
// IEEE-754 double on the way in, which is the precision loss this package exists
// to prevent. minorUnits is the exact integer form, so a consumer never has to
// parse the decimal at all.
type moneyJSON struct {
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	MinorUnits int64  `json:"minorUnits"`
}

// MarshalJSON refuses an amount with no currency rather than encoding it. The
// zero Money would otherwise marshal happily and fail on the far side, where the
// failure reads as the receiving service being broken rather than the value
// being malformed.
func (m Money) MarshalJSON() ([]byte, error) {
	if m.currency.IsZero() {
		return nil, fmt.Errorf("money: refusing to encode an amount with no currency (%d minor units)", m.minorUnits)
	}
	return json.Marshal(moneyJSON{
		Amount:     m.Decimal().StringFixed(m.currency.Exponent),
		Currency:   m.currency.Code,
		MinorUnits: m.minorUnits,
	})
}

// moneyJSONIn distinguishes an absent field from a present zero, so that
// {"minorUnits": 0} is honoured rather than mistaken for "not supplied".
type moneyJSONIn struct {
	Amount     *string `json:"amount"`
	Currency   string  `json:"currency"`
	MinorUnits *int64  `json:"minorUnits"`
}

// UnmarshalJSON requires the currency, and accepts the amount as minorUnits, as a
// major-unit decimal string, or as both - which must then agree.
//
// A disagreement is rejected rather than resolved by precedence: the caller's two
// statements of the same charge differ, which is worth surfacing loudly in a
// system that moves money.
func (m *Money) UnmarshalJSON(data []byte) error {
	var in moneyJSONIn
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("money: decoding amount: %w", err)
	}
	c, err := Lookup(in.Currency)
	if err != nil {
		return err
	}
	switch {
	case in.MinorUnits != nil:
		out := New(*in.MinorUnits, c)
		if in.Amount != nil {
			stated, err := ParseExact(*in.Amount, c)
			if err != nil {
				return err
			}
			if !stated.Equal(out) {
				return fmt.Errorf("money: amount %q and minorUnits %d disagree for %s",
					*in.Amount, *in.MinorUnits, c.Code)
			}
		}
		*m = out
		return nil
	case in.Amount != nil:
		out, err := ParseExact(*in.Amount, c)
		if err != nil {
			return err
		}
		*m = out
		return nil
	default:
		return fmt.Errorf("money: amount requires either %q or %q", "amount", "minorUnits")
	}
}
